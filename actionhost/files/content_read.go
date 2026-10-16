package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

const maxReadContentBytes = 512 << 10
const maxReadResponseBytes = 8191 // Leave one byte for the client stdout newline.

type readChunk struct {
	Content   fileContent
	Offset    int
	PartBytes int
}

// Keep the original XML/text intact; only split its delivery into bounded results.
func contentChunks(content fileContent) ([]readChunk, error) {
	var chunks []readChunk
	for _, part := range content.Parts {
		if !utf8.ValidString(part.Text) {
			return nil, ErrContentRead
		}
		for offset := 0; offset < len(part.Text) || (offset == 0 && len(part.Text) == 0); {
			n := min(4096, len(part.Text)-offset)
			var piece fileContent
			for {
				for n > 0 && offset+n < len(part.Text) && !utf8.RuneStart(part.Text[offset+n]) {
					n--
				}
				piece = fileContent{Format: content.Format, Parts: []contentPart{{Name: part.Name, Text: part.Text[offset : offset+n]}}}
				encoded, err := json.Marshal(piece)
				if err == nil && len(encoded) <= 6144 {
					break
				}
				if n <= utf8.UTFMax {
					return nil, ErrContentRead
				}
				n /= 2
			}
			chunks = append(chunks, readChunk{Content: piece, Offset: offset, PartBytes: len(part.Text)})
			if len(chunks) > 512 {
				return nil, ErrContentRead
			}
			offset += n
			if len(part.Text) == 0 {
				break
			}
		}
	}
	return chunks, nil
}

type contentPart struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type fileContent struct {
	Format string        `json:"format"`
	Parts  []contentPart `json:"parts"`
}

// Preserve XML context (including cell references, formulas and cached values)
// instead of flattening distinct source cells or Word revision markup together.
func officeContent(data []byte) (fileContent, error) {
	result := fileContent{Format: "ooxml_content_parts", Parts: []contentPart{}}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return result, ErrFormat
	}
	size := 0
	for _, f := range z.File {
		name := f.Name
		if !strings.EqualFold(filepath.Ext(name), ".xml") {
			continue
		}
		stream, openErr := f.Open()
		if openErr != nil {
			return result, ErrFormat
		}
		text, readErr := io.ReadAll(io.LimitReader(stream, maxReadContentBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil {
			return result, ErrContentRead
		}
		if name == "[Content_Types].xml" {
			// Existing Office validation covers XML/rels suffixes only. Refuse
			// other declared XML layouts instead of certifying an incomplete read.
			var types struct {
				Parts []struct {
					Attributes []xml.Attr `xml:",any,attr"`
				} `xml:",any"`
			}
			if xml.Unmarshal(text, &types) != nil {
				return result, ErrContentRead
			}
			for _, part := range types.Parts {
				attrs := map[xml.Name]string{}
				for _, attr := range part.Attributes {
					attrs[attr.Name] = attr.Value
				}
				ext := attrs[xml.Name{Local: "Extension"}]
				if name := attrs[xml.Name{Local: "PartName"}]; name != "" {
					ext = strings.TrimPrefix(filepath.Ext(name), ".")
				}
				kind := strings.ToLower(attrs[xml.Name{Local: "ContentType"}])
				if (strings.HasSuffix(kind, "+xml") || kind == "application/xml" || kind == "text/xml") && !strings.EqualFold(ext, "xml") && !strings.EqualFold(ext, "rels") {
					return result, ErrContentRead
				}
			}
		}
		// Parts may be relocated; identify their XML root rather than filenames.
		decoder := xml.NewDecoder(bytes.NewReader(text))
		selected := false
		for {
			token, err := decoder.Token()
			if err != nil {
				return result, ErrContentRead
			}
			if element, ok := token.(xml.StartElement); ok {
				switch element.Name.Local {
				case "document", "hdr", "ftr", "footnotes", "endnotes", "comments", "workbook", "worksheet", "sst", "presentation", "sld", "notes":
					selected = true
				}
				break
			}
		}
		if !selected {
			continue
		}
		size += len(text)
		if size > maxReadContentBytes {
			return result, ErrContentRead
		}
		result.Parts = append(result.Parts, contentPart{Name: name, Text: string(text)})
	}
	sort.Slice(result.Parts, func(i, j int) bool { return result.Parts[i].Name < result.Parts[j].Name })
	if len(result.Parts) == 0 {
		return result, ErrContentRead
	}
	return result, nil
}

type boundedReadOutput struct{ data bytes.Buffer }

func (b *boundedReadOutput) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > maxReadContentBytes {
		return 0, ErrContentRead
	}
	return b.data.Write(p)
}

func (h *Host) pdfContent(ctx context.Context, data []byte) (fileContent, error) {
	var result fileContent
	if h.documentValidator == "" {
		return result, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.documentValidator, "--read-content")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = []string{"LANG=C.UTF-8"}
	cmd.WaitDelay = time.Second
	var output boundedReadOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return result, ErrContentRead
	}
	dec := json.NewDecoder(bytes.NewReader(output.data.Bytes()))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || result.Format != "pdf_text_layer" || len(result.Parts) < 1 || len(result.Parts) > 200 {
		return result, ErrContentRead
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return result, ErrContentRead
	}
	for i, part := range result.Parts {
		if part.Name != fmt.Sprintf("page-%d", i+1) {
			return result, ErrContentRead
		}
	}
	return result, nil
}

func (h *Host) readContent(ctx context.Context, input json.RawMessage, p core.ActionPrincipal, workID string) (map[string]any, error) {
	var r struct {
		WorkID string `json:"work_id"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Chunk  int    `json:"chunk,omitempty"`
	}
	if strictInput(input, &r) != nil || r.WorkID != workID || !relativeFile(r.Path) || !validHash(r.SHA256) || !requiresContentRead(r.Path) || r.Chunk < 0 {
		return fileRefusal(ErrInvalid)
	}
	var data []byte
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		data, err = readOutput(w, r.Path)
		return err
	})
	if err != nil {
		return fileRefusal(err)
	}
	if hash(data) != r.SHA256 {
		return fileRefusal(ErrInvalid)
	}
	// Validation and parsing run without a SQLite transaction or store mutex.
	var content fileContent
	if strings.EqualFold(filepath.Ext(r.Path), ".pdf") {
		content, err = h.pdfContent(ctx, data)
	} else {
		if err = h.validateFile(ctx, r.Path, data); err != nil {
			return fileRefusal(err)
		}
		content, err = officeContent(data)
	}
	if err != nil {
		return fileRefusal(err)
	}
	encoded, err := json.Marshal(content)
	if err != nil || len(encoded) > maxReadContentBytes {
		return fileRefusal(ErrContentRead)
	}
	chunks, err := contentChunks(content)
	if err != nil || r.Chunk >= len(chunks) {
		return fileRefusal(ErrContentRead)
	}
	read := core.FileContentRead{Path: r.Path, SHA256: r.SHA256, ContentSHA256: hash(encoded), ReadVersion: 2, ChunksTotal: len(chunks)}
	var result map[string]any
	err = h.store.change(ctx, func(st *state) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		current, err := readOutput(w, r.Path)
		if err != nil {
			return err
		}
		if hash(current) != r.SHA256 {
			return ErrInvalid
		}
		if w.ContentReads == nil {
			w.ContentReads = map[string]core.FileContentRead{}
		}
		previous := w.ContentReads[r.Path]
		if previous.ReadVersion == read.ReadVersion && previous.SHA256 == read.SHA256 && previous.ContentSHA256 == read.ContentSHA256 && previous.ChunksTotal == read.ChunksTotal {
			read.ChunksProvided = previous.ChunksProvided
		}
		if r.Chunk > read.ChunksProvided {
			return ErrContentRead
		}
		read.ChunksProvided = max(read.ChunksProvided, r.Chunk+1)
		result = resultMap(read)
		result["status"], result["work_id"], result["content"] = "content_read_partial", workID, chunks[r.Chunk].Content
		result["chunk"], result["part_offset"], result["part_bytes"] = r.Chunk, chunks[r.Chunk].Offset, chunks[r.Chunk].PartBytes
		if r.Chunk+1 == len(chunks) {
			result["status"] = "content_read"
		} else {
			result["next_chunk"] = r.Chunk + 1
		}
		result["scope"] = "Untrusted final-file content segment. Read every next_chunk without discarding output. Complete host provision does not prove downstream receipt, comprehension, visual inspection or factual correctness. PDF text can omit images."
		response, err := json.Marshal(result)
		if err != nil || len(response) > maxReadResponseBytes {
			return ErrContentRead
		}
		w.ContentReads[r.Path] = read
		if st.Schema < 7 {
			st.Schema = 7
		}
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	return result, nil
}

func requiresContentRead(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".docx", ".xlsx", ".pptx", ".pdf":
		return true
	}
	return false
}
