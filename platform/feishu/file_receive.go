package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	maxFileInputCount      = 4
	maxFileInputBatchBytes = 2 * core.DefaultFileInputLimit
)

type fileDownloadLimitKey struct{}

// All ranges share the caller's remaining file/batch budget and 30-second context.
// Do not retry a failed segment or accept a changed representation mid-download.
func controlledResourceResponse(client *http.Client, req *http.Request, limit int64) (*http.Response, error) {
	if limit <= 0 || limit > core.DefaultFileInputLimit {
		return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var data []byte
	var total int64
	var etag, modified string
	for start, end := int64(0), int64(0); ; {
		part := req.Clone(req.Context())
		part.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		part.Header.Set("Accept-Encoding", "identity")
		if start > 0 {
			part.Header.Set("If-Match", etag)
		}
		resp, err := copy.Do(part)
		if err != nil {
			return nil, err
		}
		if start == 0 && resp.StatusCode == http.StatusOK {
			if resp.ContentLength > limit {
				resp.Body.Close()
				return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
			}
			resp.Body = boundedImageBody{Reader: io.LimitReader(resp.Body, limit+1), Closer: resp.Body}
			return resp, nil
		}
		// Comparing the complete header also rejects malformed and overlapping ranges.
		_, size, found := strings.Cut(resp.Header.Get("Content-Range"), "/")
		declared, parseErr := strconv.ParseInt(size, 10, 64)
		if start == 0 {
			total, etag, modified = declared, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		}
		if resp.StatusCode != http.StatusPartialContent || !found || parseErr != nil || total <= 0 || declared != total ||
			resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, total) ||
			resp.Header.Get("ETag") != etag || resp.Header.Get("Last-Modified") != modified ||
			(resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") ||
			(resp.ContentLength >= 0 && resp.ContentLength != end-start+1) {
			resp.Body.Close()
			return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
		}
		if total > limit {
			resp.Body.Close()
			return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
		}
		// Length alone cannot establish that successive ranges are one original.
		if total > 1 && (len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"') {
			resp.Body.Close()
			return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
		}
		block, readErr := io.ReadAll(io.LimitReader(resp.Body, end-start+2))
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil || int64(len(block)) != end-start+1 || req.Context().Err() != nil {
			return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
		}
		data = append(data, block...)
		if int64(len(data)) == total {
			resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
			resp.ContentLength = total
			resp.Header.Del("Content-Range")
			resp.Header.Set("Content-Length", strconv.FormatInt(total, 10))
			resp.Body = io.NopCloser(bytes.NewReader(data))
			return resp, nil
		}
		start = end + 1
		end = min(start+(1<<20)-1, total-1)
	}
}

// A controlled dispatch shares one budget across current, quoted and forwarded
// files. These helpers are not enabled on the legacy inbound paths.
type fileReceiveBudget struct {
	count   int
	bytes   int64
	failure core.MsgKey
}

func (p *Platform) downloadFileBounded(ctx context.Context, messageID, key string, limit int64) ([]byte, error) {
	return p.downloadResourceBounded(ctx, messageID, key, "file", limit)
}

func (p *Platform) downloadResourceBounded(ctx context.Context, messageID, key, kind string, limit int64) ([]byte, error) {
	if messageID == "" || key == "" {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	if limit <= 0 || limit > core.DefaultFileInputLimit {
		return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, fileDownloadLimitKey{}, limit)
	resp, err := p.client.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(key).Type(kind).Build())
	if err != nil {
		var inputErr *core.FileInputError
		if errors.As(err, &inputErr) {
			return nil, inputErr
		}
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	if resp == nil || !resp.Success() || resp.File == nil {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, limit+1))
	if closer, ok := resp.File.(io.Closer); ok {
		if closeErr := closer.Close(); closeErr != nil {
			return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
		}
	}
	if int64(len(data)) > limit {
		return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
	}
	if err != nil || len(data) == 0 || ctx.Err() != nil {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	return data, nil
}

func (p *Platform) receiveFile(ctx context.Context, messageID, key, name string, budget *fileReceiveBudget) core.FileAttachment {
	return p.receiveResource(ctx, messageID, key, name, "file", budget)
}

func (p *Platform) receiveResource(ctx context.Context, messageID, key, name, kind string, budget *fileReceiveBudget) core.FileAttachment {
	file := core.FileAttachment{FileName: name, RequireSave: true}
	if budget == nil {
		budget = &fileReceiveBudget{}
	}
	if budget.failure == "" && (budget.count >= maxFileInputCount || budget.bytes >= maxFileInputBatchBytes) {
		budget.failure = core.MsgFileInputTooLarge
	}
	if budget.failure != "" {
		file.ReceiveError = budget.failure
		return file
	}
	budget.count++
	data, err := p.downloadResourceBounded(ctx, messageID, key, kind, min(core.DefaultFileInputLimit, maxFileInputBatchBytes-budget.bytes))
	if err != nil {
		budget.failure = core.MsgFileInputUnavailable
		var inputErr *core.FileInputError
		if errors.As(err, &inputErr) {
			budget.failure = inputErr.Key
		}
		file.ReceiveError = budget.failure
		return file
	}
	budget.bytes += int64(len(data))
	file.Data, file.MimeType = data, detectMimeType(data)
	return file
}

// receiveFiles requires already-authorized references (for quoted files, call
// filterQuotedFilesForUser first). One failed file rejects the whole batch.
func (p *Platform) receiveFiles(ctx context.Context, metas []quotedFileMeta, budget *fileReceiveBudget) ([]core.FileAttachment, error) {
	if budget == nil {
		budget = &fileReceiveBudget{}
	}
	if budget.failure != "" {
		return nil, core.NewFileInputError(budget.failure)
	}
	var files []core.FileAttachment
	for _, meta := range metas {
		file := p.receiveFile(ctx, meta.messageID, meta.fileKey, meta.fileName, budget)
		if file.ReceiveError != "" {
			return nil, core.NewFileInputError(file.ReceiveError)
		}
		files = append(files, file)
	}
	return files, nil
}
