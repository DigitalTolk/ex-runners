package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// MR creation ----------------------------------------------------------------

// MergeRequestInput is one MR to open (or find) on GitLab.
type MergeRequestInput struct {
	Host         string   `json:"host"`    // GitLab origin
	APIBase      string   `json:"apiBase"` // connector base URL (…/api/v4)
	Token        string   `json:"token"`
	ProjectPath  string   `json:"projectPath"`
	SourceBranch string   `json:"sourceBranch"`
	TargetBranch string   `json:"targetBranch"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Labels       []string `json:"labels,omitempty"`
}

// MergeRequestResult is the MR that now exists for the branch.
type MergeRequestResult struct {
	URL     string `json:"url"`
	IID     int    `json:"iid"`
	Existed bool   `json:"existed"` // an open MR for the branch was already there
}

// mrCreateBody is the POST body, in the TS runner's key order.
type mrCreateBody struct {
	SourceBranch       string  `json:"source_branch"`
	TargetBranch       string  `json:"target_branch"`
	Title              string  `json:"title"`
	Description        string  `json:"description"`
	RemoveSourceBranch bool    `json:"remove_source_branch"`
	Squash             bool    `json:"squash"`
	Labels             *string `json:"labels,omitempty"`
}

// CreateMergeRequest opens (or finds) the MR for the branch via the GitLab
// API using the requester's own token — the MR author is the requester; the
// description carries the Ex signature.
func CreateMergeRequest(ctx context.Context, in MergeRequestInput) (MergeRequestResult, error) {
	api := strings.TrimRight(in.APIBase, "/")
	project := encodeURIComponent(in.ProjectPath)
	// Idempotent: an open MR for this branch is THE MR.
	status, body, err := gitlabRequest(ctx, http.MethodGet,
		api+"/projects/"+project+"/merge_requests?source_branch="+encodeURIComponent(in.SourceBranch)+"&state=opened&per_page=1",
		in.Token, nil)
	if err != nil {
		return MergeRequestResult{}, err
	}
	if status >= 200 && status < 300 {
		var rows any
		if err := json.Unmarshal(body, &rows); err != nil {
			return MergeRequestResult{}, err
		}
		if list, ok := rows.([]any); ok && len(list) > 0 {
			if url, iid := mrFields(list[0]); url != "" {
				return MergeRequestResult{URL: url, IID: iid, Existed: true}, nil
			}
		}
	}
	req := mrCreateBody{
		SourceBranch: in.SourceBranch,
		TargetBranch: in.TargetBranch,
		Title:        in.Title,
		Description:  in.Description,

		RemoveSourceBranch: true,
		Squash:             false,
	}
	if len(in.Labels) > 0 {
		labels := strings.Join(in.Labels, ",")
		req.Labels = &labels
	}
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	enc.SetEscapeHTML(false) // byte-for-byte JSON.stringify for titles with < > &
	_ = enc.Encode(req)      // strings and bools: encoding cannot fail
	status, body, err = gitlabRequest(ctx, http.MethodPost, api+"/projects/"+project+"/merge_requests",
		in.Token, bytes.TrimSuffix(payload.Bytes(), []byte("\n")))
	if err != nil {
		return MergeRequestResult{}, err
	}
	text := string(body)
	if status < 200 || status >= 300 {
		return MergeRequestResult{}, fmt.Errorf("GitLab refused the merge request (HTTP %d): %s", status, textutil.Head(text, 600))
	}
	var mr any
	if err := json.Unmarshal(body, &mr); err != nil {
		return MergeRequestResult{}, err
	}
	url, iid := mrFields(mr)
	if url == "" {
		return MergeRequestResult{}, errors.New("GitLab returned no MR URL")
	}
	return MergeRequestResult{URL: url, IID: iid, Existed: false}, nil
}

// gitlabRequest sends one API call with the requester's token and returns
// the status and body.
func gitlabRequest(ctx context.Context, method, url, token string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	return res.StatusCode, data, err
}

// mrFields reads web_url and iid (iid ?? 0) off a decoded MR object.
func mrFields(v any) (string, int) {
	m, _ := v.(map[string]any)
	url, _ := m["web_url"].(string)
	iid, _ := m["iid"].(float64)
	return url, int(iid)
}

// encodeURIComponent is JavaScript's: every UTF-8 byte percent-encoded
// except A–Z a–z 0–9 - _ . ! ~ * ' ( ).
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
