package workspace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type stubResp struct {
	status int
	body   string
}

type recordedReq struct {
	method, uri, auth, accept, contentType, body string
}

// stubGitLab serves the scripted responses in order and records every request.
func stubGitLab(t *testing.T, responses ...stubResp) (*httptest.Server, func() []recordedReq) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		i := len(reqs)
		reqs = append(reqs, recordedReq{r.Method, r.RequestURI, r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		if i >= len(responses) {
			t.Errorf("unexpected request %d: %s %s", i, r.Method, r.RequestURI)
			w.WriteHeader(599)
			return
		}
		w.WriteHeader(responses[i].status)
		_, _ = io.WriteString(w, responses[i].body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedReq(nil), reqs...)
	}
}

func mrInput(srv *httptest.Server) MergeRequestInput {
	return MergeRequestInput{
		Host:         srv.URL,
		APIBase:      srv.URL + "/api/v4/",
		Token:        "glpat-x",
		ProjectPath:  "dt/booking-portal",
		SourceBranch: "ex/task-1",
		TargetBranch: "main",
		Title:        "fix: leap year",
		Description:  "via ex",
	}
}

func TestFindsAnOpenMRForTheBranchAndReusesIt(t *testing.T) {
	srv, reqs := stubGitLab(t, stubResp{200, `[{"web_url":"https://gl/mr/7","iid":7}]`})
	got, err := CreateMergeRequest(context.Background(), mrInput(srv))
	if err != nil || got != (MergeRequestResult{URL: "https://gl/mr/7", IID: 7, Existed: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	srv2, _ := stubGitLab(t, stubResp{200, `[{"web_url":"https://gl/mr/7"}]`})
	got, err = CreateMergeRequest(context.Background(), mrInput(srv2))
	if err != nil || got != (MergeRequestResult{URL: "https://gl/mr/7", IID: 0, Existed: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Trailing slash on apiBase is normalized; the branch is URL-encoded.
	r := reqs()[0]
	if r.method != http.MethodGet || r.uri != "/api/v4/projects/dt%2Fbooking-portal/merge_requests?source_branch=ex%2Ftask-1&state=opened&per_page=1" {
		t.Fatalf("lookup = %s %s", r.method, r.uri)
	}
	if r.auth != "Bearer glpat-x" || r.accept != "application/json" || r.contentType != "application/json" {
		t.Fatalf("headers = %+v", r)
	}
}

func TestCreatesTheMRWhenNoneIsOpenWithLabelsWhenGiven(t *testing.T) {
	srv, reqs := stubGitLab(t, stubResp{200, `[]`}, stubResp{201, `{"web_url":"https://gl/mr/8","iid":8}`})
	in := mrInput(srv)
	in.Labels = []string{"ex", "bot"}
	got, err := CreateMergeRequest(context.Background(), in)
	if err != nil || got != (MergeRequestResult{URL: "https://gl/mr/8", IID: 8, Existed: false}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	post := reqs()[1]
	var body map[string]any
	if err := json.Unmarshal([]byte(post.body), &body); err != nil {
		t.Fatal(err)
	}
	if body["labels"] != "ex,bot" || body["remove_source_branch"] != true {
		t.Fatalf("body = %v", body)
	}
	if post.method != http.MethodPost || post.uri != "/api/v4/projects/dt%2Fbooking-portal/merge_requests" || post.auth != "Bearer glpat-x" || post.contentType != "application/json" {
		t.Fatalf("create = %+v", post)
	}
	// Byte-for-byte what JSON.stringify sends.
	want := `{"source_branch":"ex/task-1","target_branch":"main","title":"fix: leap year","description":"via ex","remove_source_branch":true,"squash":false,"labels":"ex,bot"}`
	if post.body != want {
		t.Fatalf("body =\n%s\nwant\n%s", post.body, want)
	}
}

func TestStillCreatesWhenTheLookupItselfFailsAndDefaultsIIDToZero(t *testing.T) {
	srv, reqs := stubGitLab(t, stubResp{500, "boom"}, stubResp{201, `{"web_url":"https://gl/mr/9"}`})
	got, err := CreateMergeRequest(context.Background(), mrInput(srv))
	if err != nil || got != (MergeRequestResult{URL: "https://gl/mr/9", IID: 0, Existed: false}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// No labels: the key is absent, and < > & go out unescaped.
	if strings.Contains(reqs()[1].body, "labels") {
		t.Fatalf("body = %s", reqs()[1].body)
	}
}

func TestSurfacesARefusedCreateWithTheResponseTextAndAMissingURL(t *testing.T) {
	srv, _ := stubGitLab(t, stubResp{200, `[{}]`}, stubResp{409, "branch conflict"})
	_, err := CreateMergeRequest(context.Background(), mrInput(srv))
	if err == nil || err.Error() != "GitLab refused the merge request (HTTP 409): branch conflict" {
		t.Fatalf("err = %v", err)
	}
	srv2, _ := stubGitLab(t, stubResp{200, `"not-an-array"`}, stubResp{201, `{"iid":3}`})
	_, err = CreateMergeRequest(context.Background(), mrInput(srv2))
	if err == nil || err.Error() != "GitLab returned no MR URL" {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusalTextIsClippedTo600Characters(t *testing.T) {
	long := strings.Repeat("ü", 700)
	srv, _ := stubGitLab(t, stubResp{200, `[]`}, stubResp{422, long})
	_, err := CreateMergeRequest(context.Background(), mrInput(srv))
	if err == nil || err.Error() != "GitLab refused the merge request (HTTP 422): "+strings.Repeat("ü", 600) {
		t.Fatalf("err = %v", err)
	}
}

func TestTitleAndDescriptionAreSentUnescaped(t *testing.T) {
	srv, reqs := stubGitLab(t, stubResp{404, ``}, stubResp{201, `{"web_url":"u","iid":1.0}`})
	in := mrInput(srv)
	in.Title = "fix: a < b && c > d"
	in.Description = "line1\n\n---\n🤖 Ex"
	in.Labels = []string{}
	if _, err := CreateMergeRequest(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if b := reqs()[1].body; !strings.Contains(b, `"title":"fix: a < b && c > d"`) || !strings.Contains(b, `"description":"line1\n\n---\n🤖 Ex"`) || strings.Contains(b, "labels") {
		t.Fatalf("body = %s", b)
	}
}

func TestMalformedJSONFailsLikeResponseJSON(t *testing.T) {
	// An OK lookup whose body is not JSON rejects (TS: existing.json() throws).
	srv, reqs := stubGitLab(t, stubResp{200, `{not json`})
	if _, err := CreateMergeRequest(context.Background(), mrInput(srv)); err == nil {
		t.Fatal("want lookup JSON error")
	}
	if len(reqs()) != 1 {
		t.Fatal("must not create after a broken lookup")
	}
	// An OK create whose body is not JSON rejects (TS: JSON.parse throws).
	srv2, _ := stubGitLab(t, stubResp{200, `[]`}, stubResp{201, `<html>`})
	if _, err := CreateMergeRequest(context.Background(), mrInput(srv2)); err == nil {
		t.Fatal("want create JSON error")
	}
}

func TestTransportFailuresSurface(t *testing.T) {
	// Unreachable API: the lookup itself fails.
	srv, _ := stubGitLab(t)
	in := mrInput(srv)
	srv.Close()
	if _, err := CreateMergeRequest(context.Background(), in); err == nil {
		t.Fatal("want connection error")
	}
	// An API base that is not a URL.
	in.APIBase = "http://[::1"
	if _, err := CreateMergeRequest(context.Background(), in); err == nil {
		t.Fatal("want URL error")
	}
	// The create request dies mid-flight.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer hang.Close()
	if _, err := CreateMergeRequest(context.Background(), mrInput(hang)); err == nil {
		t.Fatal("want create transport error")
	}
	// A truncated response body.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, _ := w.(http.Hijacker).Hijack()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n[]")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer short.Close()
	if _, err := CreateMergeRequest(context.Background(), mrInput(short)); err == nil {
		t.Fatal("want body read error")
	}
	// A cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv3, _ := stubGitLab(t)
	if _, err := CreateMergeRequest(ctx, mrInput(srv3)); err == nil {
		t.Fatal("want context error")
	}
}

func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	// Expected values recorded from Node's encodeURIComponent.
	for in, want := range map[string]string{
		"dt/booking-portal": "dt%2Fbooking-portal",
		"ex/task-1":         "ex%2Ftask-1",
		"a b&c=d+e":         "a%20b%26c%3Dd%2Be",
		"!~*()'x":           "!~*()'x",
		"ü€😀":               "%C3%BC%E2%82%AC%F0%9F%98%80",
		":@$,;?#[]":         "%3A%40%24%2C%3B%3F%23%5B%5D",
	} {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
