package feishu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// Leave margin inside Feishu's one-hour UUID window; retries share this deadline.
const messageSendTimeout = 55 * time.Minute

type messageSendOutcome struct{ unknown error }

func (o *messageSendOutcome) unconfirmed(err error) error {
	if o.unknown == nil {
		o.unknown = err
	}
	return err
}

func (o *messageSendOutcome) response(operation string, response *larkcore.ApiResp, code int, msg string) error {
	if response == nil || response.StatusCode >= http.StatusInternalServerError || (response.StatusCode != http.StatusOK && code == 0) {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return o.unconfirmed(fmt.Errorf("%s: response does not confirm message delivery status=%d code=%d msg=%s", operation, status, code, msg))
	}
	if code != 0 {
		err := fmt.Errorf("%s failed code=%d msg=%s", operation, code, msg)
		if code == 230049 {
			return o.unconfirmed(err)
		}
		return err
	}
	return nil
}

func (o *messageSendOutcome) result(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if o.unknown != nil {
		return errors.Join(core.ErrMessageDeliveryUnconfirmed, o.unknown, err)
	}
	return err
}
