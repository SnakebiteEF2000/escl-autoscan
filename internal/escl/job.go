package escl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CreateJob posts a scan job and returns the job URL.
//
// A busy device answers 503 or 409; that is reported as ErrBusy.
func (c *Client) CreateJob(ctx context.Context, s ScanSettings) (string, error) {
	resp, err := c.request(ctx, c.o.Timeout, http.MethodPost, c.endpoint("/eSCL/ScanJobs"), s.marshal(), 64<<10)
	if err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}
	switch resp.code {
	case http.StatusCreated:
	case http.StatusServiceUnavailable, http.StatusConflict:
		return "", fmt.Errorf("create job: HTTP %d: %w", resp.code, ErrBusy)
	default:
		return "", &StatusError{Op: "create job", Code: resp.code}
	}
	job, err := c.jobURL(resp.header.Get("Location"))
	if err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}
	return job, nil
}

// jobURL turns the Location of a new job into a URL on the configured base.
//
// The WF-2960 returns an absolute Location (https://<ip>:443/eSCL/ScanJobs/<uuid>),
// other devices a relative one. Only the path is used, so requests keep going
// to the configured host and port (and keep the Host header the device needs).
func (c *Client) jobURL(location string) (string, error) {
	if location == "" {
		return "", errors.New("response has no Location header")
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("invalid Location %q: %w", location, err)
	}
	ref := &url.URL{Path: loc.Path, RawQuery: loc.RawQuery}
	if loc.Path == "" {
		return "", fmt.Errorf("invalid Location %q: no path", location)
	}
	u := c.base.ResolveReference(ref)
	return u.String(), nil
}

// NextDocument fetches the next document of a job.
//
// It returns ErrNoMoreDocuments (404), ErrJobAborted (410) or ErrNotReady
// (503) for the respective statuses. If reading a 200 body fails, the partial
// body is returned together with the error.
func (c *Client) NextDocument(ctx context.Context, job string) ([]byte, error) {
	resp, err := c.request(ctx, c.o.DocumentTimeout, http.MethodGet, job+"/NextDocument", nil, c.o.MaxDocumentBytes)
	if resp != nil && resp.code == http.StatusOK && err != nil {
		return resp.body, fmt.Errorf("next document: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("next document: %w", err)
	}
	switch resp.code {
	case http.StatusOK:
		return resp.body, nil
	case http.StatusNotFound:
		return nil, ErrNoMoreDocuments
	case http.StatusGone:
		return nil, ErrJobAborted
	case http.StatusServiceUnavailable:
		return nil, ErrNotReady
	default:
		return nil, &StatusError{Op: "next document", Code: resp.code}
	}
}

// Documents fetches documents of a job until the device reports that there
// are no more (404). It retries while the device answers 503.
//
// Every document received is returned, also when err is not nil, so callers
// can keep a partial scan for inspection. A nil error only means the job ran
// to its normal end; the documents still have to be validated.
func (c *Client) Documents(ctx context.Context, job string) ([][]byte, error) {
	var docs [][]byte
	notReady := 0
	for {
		doc, err := c.NextDocument(ctx, job)
		switch {
		case err == nil:
			docs = append(docs, doc)
			notReady = 0
		case errors.Is(err, ErrNoMoreDocuments):
			return docs, nil
		case errors.Is(err, ErrNotReady):
			notReady++
			if notReady > c.o.Retries {
				return docs, fmt.Errorf("next document: still not ready after %d retries: %w", c.o.Retries, err)
			}
			if err := sleep(ctx, c.o.RetryInterval); err != nil {
				return docs, err
			}
		default:
			if len(doc) > 0 {
				docs = append(docs, doc)
			}
			return docs, err
		}
	}
}

// Delete deletes (cancels) a job. A 404 is fine: the WF-2960 discards a job
// on its own once the last document has been fetched.
func (c *Client) Delete(ctx context.Context, job string) error {
	resp, err := c.request(ctx, c.o.Timeout, http.MethodDelete, job, nil, 64<<10)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	switch resp.code {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusGone:
		return nil
	}
	return &StatusError{Op: "delete job", Code: resp.code}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
