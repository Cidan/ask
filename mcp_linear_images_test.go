package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// -----------------------------------------------------------------------
// Pure helpers — URL extraction + host gate
// -----------------------------------------------------------------------

func TestIsLinearImageURL_AcceptsAndRejects(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://uploads.linear.app/abc/img.png", true},
		{"https://UPLOADS.LINEAR.APP/abc/img.png", true},
		// http:// is refused so the auth token is never sent in plaintext,
		// even when the host is otherwise legitimate.
		{"http://uploads.linear.app/abc/img.png", false},
		{"https://uploads.linear.app/abc/img.png?token=x", true},
		{"https://example.com/abc.png", false},
		{"https://uploads.linear.app.evil.com/img.png", false},
		{"file:///tmp/local.png", false},
		{"", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isLinearImageURL(tc.in); got != tc.want {
				t.Errorf("isLinearImageURL(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLinearImageCheckRedirect_BlocksOffHostRedirects(t *testing.T) {
	mkReq := func(raw string) *http.Request {
		req, _ := http.NewRequest("GET", raw, nil)
		return req
	}
	// Same host, https — allowed.
	if err := linearImageCheckRedirect(mkReq("https://uploads.linear.app/a/2.png"), nil); err != nil {
		t.Errorf("same-host redirect blocked: %v", err)
	}
	// Off-host redirect — blocked.
	err := linearImageCheckRedirect(mkReq("https://evil.example.com/x.png"), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked redirect") {
		t.Errorf("off-host redirect not blocked: %v", err)
	}
	// Downgrade to http — blocked (isLinearImageURL refuses http).
	err = linearImageCheckRedirect(mkReq("http://uploads.linear.app/a/p.png"), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked redirect") {
		t.Errorf("plaintext redirect not blocked: %v", err)
	}
	// Too many hops — blocked.
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = mkReq("https://uploads.linear.app/b")
	}
	err = linearImageCheckRedirect(mkReq("https://uploads.linear.app/c"), via)
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("redirect cap not enforced: %v", err)
	}
}

func TestLinearExtractImageURLs_DescriptionAndComments(t *testing.T) {
	desc := strings.Join([]string{
		"# Repro",
		"![screenshot](https://uploads.linear.app/a/one.png)",
		"some prose",
		"![titled](https://uploads.linear.app/a/two.png \"caption\")",
		"![external](https://i.imgur.com/x.png)",
		"plain link https://uploads.linear.app/a/three.png does NOT count",
	}, "\n")
	comments := []string{
		"![ditto](https://uploads.linear.app/a/one.png)", // dup of desc
		"![comment-only](https://uploads.linear.app/c/four.png)",
		"no images here",
	}
	got := linearExtractImageURLs(desc, comments)
	wantURLs := []string{
		"https://uploads.linear.app/a/one.png",
		"https://uploads.linear.app/a/two.png",
		"https://uploads.linear.app/c/four.png",
	}
	if len(got) != len(wantURLs) {
		t.Fatalf("got %d refs, want %d (%+v)", len(got), len(wantURLs), got)
	}
	for i, w := range wantURLs {
		if got[i].URL != w {
			t.Errorf("refs[%d].URL=%q want %q", i, got[i].URL, w)
		}
	}
	if got[0].Source != "description" || got[1].Source != "description" {
		t.Errorf("description refs lost source tag: %+v", got[:2])
	}
	if got[2].Source != "comment" {
		t.Errorf("comment ref lost source tag: %+v", got[2])
	}
}

func TestLinearExtractImageURLs_EmptyInputs(t *testing.T) {
	if refs := linearExtractImageURLs("", nil); len(refs) != 0 {
		t.Errorf("empty inputs returned refs: %+v", refs)
	}
	if refs := linearExtractImageURLs("no images here", []string{"nor here"}); len(refs) != 0 {
		t.Errorf("text-only inputs returned refs: %+v", refs)
	}
}

func TestLinearCommentBodies_ExtractsBodies(t *testing.T) {
	in := []issueComment{
		{author: "a", body: "first"},
		{author: "b", body: "second"},
	}
	got := linearCommentBodies(in)
	want := []string{"first", "second"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("bodies[%d]=%q want %q", i, got[i], w)
		}
	}
	if got := linearCommentBodies(nil); got != nil {
		t.Errorf("nil input returned %+v want nil", got)
	}
}

// -----------------------------------------------------------------------
// linearGatherImages — caps + error handling via stub fetcher
// -----------------------------------------------------------------------

type fakeLinearImageFetcher struct {
	responses map[string]fakeImageResponse
	calls     []string
}

type fakeImageResponse struct {
	data []byte
	mime string
	err  error
}

func (f *fakeLinearImageFetcher) fetchImage(_ context.Context, _ linearMCPConfig, rawURL string) ([]byte, string, error) {
	f.calls = append(f.calls, rawURL)
	r, ok := f.responses[rawURL]
	if !ok {
		return nil, "", fmt.Errorf("no fake response for %q", rawURL)
	}
	return r.data, r.mime, r.err
}

func TestLinearGatherImages_HappyPath(t *testing.T) {
	refs := []linearImageRef{
		{URL: "https://uploads.linear.app/a/1.png", Source: "description"},
		{URL: "https://uploads.linear.app/a/2.png", Source: "description"},
	}
	f := &fakeLinearImageFetcher{responses: map[string]fakeImageResponse{
		"https://uploads.linear.app/a/1.png": {data: []byte("PNGDATA1"), mime: "image/png"},
		"https://uploads.linear.app/a/2.png": {data: []byte("JPGDATA2"), mime: "image/jpeg"},
	}}
	images, dropped := linearGatherImages(context.Background(), f, linearMCPConfig{}, refs)
	if dropped != 0 {
		t.Errorf("dropped=%d want 0", dropped)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
	if string(images[0].Data) != "PNGDATA1" || images[0].MIMEType != "image/png" {
		t.Errorf("images[0]=%+v", images[0])
	}
	if string(images[1].Data) != "JPGDATA2" || images[1].MIMEType != "image/jpeg" {
		t.Errorf("images[1]=%+v", images[1])
	}
}

func TestLinearGatherImages_CountCapDropsExcess(t *testing.T) {
	refs := make([]linearImageRef, linearImageMaxCount+2)
	resp := map[string]fakeImageResponse{}
	for i := range refs {
		u := fmt.Sprintf("https://uploads.linear.app/img/%d.png", i)
		refs[i] = linearImageRef{URL: u, Source: "description"}
		resp[u] = fakeImageResponse{data: []byte("data"), mime: "image/png"}
	}
	f := &fakeLinearImageFetcher{responses: resp}
	images, dropped := linearGatherImages(context.Background(), f, linearMCPConfig{}, refs)
	if len(images) != linearImageMaxCount {
		t.Errorf("kept=%d want %d", len(images), linearImageMaxCount)
	}
	if dropped != 2 {
		t.Errorf("dropped=%d want 2", dropped)
	}
}

func TestLinearGatherImages_ByteCapDropsLastImage(t *testing.T) {
	// First image hugs the budget. Second image would overflow and is
	// dropped without aborting the call.
	big := make([]byte, linearImageMaxBytesTotal-100)
	for i := range big {
		big[i] = 'A'
	}
	overflow := make([]byte, 200)
	refs := []linearImageRef{
		{URL: "https://uploads.linear.app/a/big.png", Source: "description"},
		{URL: "https://uploads.linear.app/a/overflow.png", Source: "description"},
	}
	f := &fakeLinearImageFetcher{responses: map[string]fakeImageResponse{
		"https://uploads.linear.app/a/big.png":      {data: big, mime: "image/png"},
		"https://uploads.linear.app/a/overflow.png": {data: overflow, mime: "image/png"},
	}}
	images, dropped := linearGatherImages(context.Background(), f, linearMCPConfig{}, refs)
	if len(images) != 1 {
		t.Errorf("kept=%d want 1", len(images))
	}
	if dropped != 1 {
		t.Errorf("dropped=%d want 1", dropped)
	}
}

func TestLinearGatherImages_PerImageCapDropsOversized(t *testing.T) {
	huge := make([]byte, linearImageMaxBytesEach+1)
	refs := []linearImageRef{
		{URL: "https://uploads.linear.app/a/huge.png", Source: "description"},
		{URL: "https://uploads.linear.app/a/small.png", Source: "description"},
	}
	f := &fakeLinearImageFetcher{responses: map[string]fakeImageResponse{
		"https://uploads.linear.app/a/huge.png":  {data: huge, mime: "image/png"},
		"https://uploads.linear.app/a/small.png": {data: []byte("ok"), mime: "image/png"},
	}}
	images, dropped := linearGatherImages(context.Background(), f, linearMCPConfig{}, refs)
	if len(images) != 1 || string(images[0].Data) != "ok" {
		t.Errorf("kept=%+v want only the small image", images)
	}
	if dropped != 1 {
		t.Errorf("dropped=%d want 1", dropped)
	}
}

func TestLinearGatherImages_FetchErrorCountedAsDropped(t *testing.T) {
	refs := []linearImageRef{
		{URL: "https://uploads.linear.app/a/ok.png", Source: "description"},
		{URL: "https://uploads.linear.app/a/bad.png", Source: "description"},
	}
	f := &fakeLinearImageFetcher{responses: map[string]fakeImageResponse{
		"https://uploads.linear.app/a/ok.png":  {data: []byte("ok"), mime: "image/png"},
		"https://uploads.linear.app/a/bad.png": {err: errors.New("transient")},
	}}
	images, dropped := linearGatherImages(context.Background(), f, linearMCPConfig{}, refs)
	if len(images) != 1 || string(images[0].Data) != "ok" {
		t.Errorf("kept=%+v want only ok.png", images)
	}
	if dropped != 1 {
		t.Errorf("dropped=%d want 1", dropped)
	}
}

func TestLinearGatherImages_NilFetcher(t *testing.T) {
	refs := []linearImageRef{{URL: "https://uploads.linear.app/a/1.png", Source: "description"}}
	images, dropped := linearGatherImages(context.Background(), nil, linearMCPConfig{}, refs)
	if len(images) != 0 || dropped != 0 {
		t.Errorf("nil fetcher: images=%+v dropped=%d", images, dropped)
	}
}

// -----------------------------------------------------------------------
// linearBuildIssueResult — Content slice assembly
// -----------------------------------------------------------------------

func TestLinearBuildIssueResult_TextOnlyWhenNoImages(t *testing.T) {
	f := &fakeLinearImageFetcher{}
	res := linearBuildIssueResult(context.Background(), f, linearMCPConfig{}, `{"foo":"bar"}`, "no images here", []string{"or here"})
	if len(res.Content) != 1 {
		t.Fatalf("content=%d blocks, want 1 (%+v)", len(res.Content), res.Content)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != `{"foo":"bar"}` {
		t.Errorf("content[0]=%+v", res.Content[0])
	}
	if len(f.calls) != 0 {
		t.Errorf("fetcher called with no images present: %+v", f.calls)
	}
}

func TestLinearBuildIssueResult_AppendsImages(t *testing.T) {
	desc := "![](https://uploads.linear.app/a/1.png)\n![](https://uploads.linear.app/a/2.png)"
	f := &fakeLinearImageFetcher{responses: map[string]fakeImageResponse{
		"https://uploads.linear.app/a/1.png": {data: []byte("AAA"), mime: "image/png"},
		"https://uploads.linear.app/a/2.png": {data: []byte("BBB"), mime: "image/gif"},
	}}
	res := linearBuildIssueResult(context.Background(), f, linearMCPConfig{}, "TEXT", desc, nil)
	if len(res.Content) != 3 {
		t.Fatalf("content=%d blocks, want 3 (%+v)", len(res.Content), res.Content)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "TEXT" {
		t.Errorf("content[0]=%+v", res.Content[0])
	}
	img1, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || string(img1.Data) != "AAA" || img1.MIMEType != "image/png" {
		t.Errorf("content[1]=%+v", res.Content[1])
	}
	img2, ok := res.Content[2].(*mcp.ImageContent)
	if !ok || string(img2.Data) != "BBB" || img2.MIMEType != "image/gif" {
		t.Errorf("content[2]=%+v", res.Content[2])
	}
}

func TestLinearBuildIssueResult_DroppedNoteSurfaces(t *testing.T) {
	// Five Linear images; cap is 4; expect text + 4 images + 1 trailing note.
	var sb strings.Builder
	resp := map[string]fakeImageResponse{}
	for i := 0; i < linearImageMaxCount+1; i++ {
		u := fmt.Sprintf("https://uploads.linear.app/x/%d.png", i)
		sb.WriteString(fmt.Sprintf("![](%s)\n", u))
		resp[u] = fakeImageResponse{data: []byte("data"), mime: "image/png"}
	}
	f := &fakeLinearImageFetcher{responses: resp}
	res := linearBuildIssueResult(context.Background(), f, linearMCPConfig{}, "T", sb.String(), nil)
	wantBlocks := 1 + linearImageMaxCount + 1
	if len(res.Content) != wantBlocks {
		t.Fatalf("content=%d blocks, want %d", len(res.Content), wantBlocks)
	}
	tail, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	if !ok {
		t.Fatalf("trailing block is not text: %+v", res.Content[len(res.Content)-1])
	}
	if !strings.Contains(tail.Text, "1 image") || !strings.Contains(tail.Text, "omitted") {
		t.Errorf("trailing note=%q does not mention the drop", tail.Text)
	}
}

// -----------------------------------------------------------------------
// linearFetchImageOverHTTP — auth + MIME + cap behaviors
// -----------------------------------------------------------------------

func TestLinearFetchImageOverHTTP_AttachesAuthAndDetectsMIME(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write([]byte("PNG"))
	}))
	defer srv.Close()
	cli := &http.Client{Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: "lin_api_x"}}
	data, mime, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "lin_api_x" {
		t.Errorf("Authorization=%q want bare key", gotAuth)
	}
	if string(data) != "PNG" {
		t.Errorf("data=%q want PNG", data)
	}
	if mime != "image/png" {
		t.Errorf("mime=%q want image/png (charset stripped)", mime)
	}
}

func TestLinearFetchImageOverHTTP_DetectsMIMEFromBodyWhenHeaderMissing(t *testing.T) {
	// PNG magic header — http.DetectContentType returns "image/png" for this.
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	cli := &http.Client{}
	_, mime, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime=%q want image/png (detected from body)", mime)
	}
}

func TestLinearFetchImageOverHTTP_RejectsNonImageContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()
	cli := &http.Client{}
	_, _, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "non-image") {
		t.Errorf("err=%v want non-image rejection", err)
	}
}

func TestLinearFetchImageOverHTTP_HTTPErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	cli := &http.Client{}
	_, _, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err=%v want HTTP 403 surfaced", err)
	}
}

// Defense-in-depth: even with CheckRedirect refusing off-host hops,
// a 30x that somehow lands on the response (e.g. CheckRedirect
// returning ErrUseLastResponse) should still fail rather than be
// treated as a successful body.
func TestLinearFetchImageOverHTTP_RejectsThreeXXResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example.com/x.png")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	cli := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	_, _, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("err=%v want HTTP 302 surfaced", err)
	}
}

func TestLinearFetchImageOverHTTP_CapsResponseSize(t *testing.T) {
	// Stream way more than the cap to ensure io.LimitReader is wired up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		chunk := make([]byte, 1024)
		for i := range chunk {
			chunk[i] = 'X'
		}
		// Write 8 MB.
		for i := 0; i < 8*1024; i++ {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()
	cli := &http.Client{}
	data, _, err := linearFetchImageOverHTTP(context.Background(), cli, srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(data) > linearImageMaxBytesEach+1 {
		t.Errorf("len(data)=%d exceeds cap+1=%d — io.LimitReader not wired", len(data), linearImageMaxBytesEach+1)
	}
}

// -----------------------------------------------------------------------
// (*linearIssueProvider).fetchImage host gate
// -----------------------------------------------------------------------

func TestLinearIssueProvider_FetchImage_RejectsNonLinearHost(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Token: "lin_api_x"}
	_, _, err := p.fetchImage(context.Background(), cfg, "https://evil.example.com/x.png")
	if err == nil || !strings.Contains(err.Error(), "non-linear host") {
		t.Errorf("err=%v want host-gate refusal", err)
	}
}

// End-to-end: a 30x from uploads.linear.app pointing at a third-party
// host must fail BEFORE the auth header reaches that host. Failing
// open here would leak the Linear API key to whatever the CDN
// redirects to.
func TestLinearIssueProvider_FetchImage_RefusesOffHostRedirect(t *testing.T) {
	var leaked string
	finalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("LEAK"))
	}))
	defer finalSrv.Close()

	uploadsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalSrv.URL, http.StatusFound)
	}))
	defer uploadsSrv.Close()

	rewriter := &hostRewriteTransport{from: "uploads.linear.app", to: uploadsSrv.Listener.Addr().String()}
	p := mcpLinearProvider()
	p.mu.Lock()
	p.httpClient = &http.Client{
		Transport: &linearAPIKeyRoundTripper{base: rewriter, token: "lin_api_x"},
		Timeout:   linearGraphQLCallTimeout,
	}
	p.cachedEndpoint = "https://api.linear.app/graphql"
	p.cachedToken = "lin_api_x"
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		p.httpClient = nil
		p.cachedEndpoint = ""
		p.cachedToken = ""
		p.mu.Unlock()
	})

	_, _, err := p.fetchImage(context.Background(), linearMCPConfig{Token: "lin_api_x"}, "https://uploads.linear.app/abc/img.png")
	if err == nil {
		t.Fatal("expected redirect-block error, got nil")
	}
	if !strings.Contains(err.Error(), "blocked redirect") && !strings.Contains(err.Error(), "host gate") {
		t.Errorf("err=%v want host-gate redirect rejection", err)
	}
	if leaked != "" {
		t.Errorf("Authorization leaked to off-host hop: %q — fetch should have failed before this handler ran", leaked)
	}
}

// -----------------------------------------------------------------------
// End-to-end: linearGetTool inlines images from a Linear-hosted URL
// -----------------------------------------------------------------------

func TestLinearGetTool_InlinesImagesFromDescription(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{
			"issue": map[string]any{
				"number":      9,
				"title":       "with image",
				"description": "![scrn](https://uploads.linear.app/a/scrn.png)",
				"state":       map[string]any{"type": "started"},
				"createdAt":   "2026-01-20T00:00:00Z",
				"comments":    map[string]any{"nodes": []any{}},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())

	// Stub the provider's image fetcher by reaching into the singleton.
	// The MCP handler calls mcpLinearProvider() which returns the
	// registry-backed *linearIssueProvider. We swap its httpClient to
	// point at a one-handler test server so the host gate passes and
	// the wire-level path is exercised end-to-end.
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm the auth header survives the round-trip.
		if got := r.Header.Get("Authorization"); got != "lin_api_x" {
			t.Errorf("image GET Authorization=%q want lin_api_x", got)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("IMG-BYTES"))
	}))
	defer imgSrv.Close()

	// Wire a transport that rewrites uploads.linear.app to the test server.
	rewriter := &hostRewriteTransport{from: "uploads.linear.app", to: imgSrv.Listener.Addr().String()}
	cli := &http.Client{Transport: &linearAPIKeyRoundTripper{base: rewriter, token: "lin_api_x"}}
	p := mcpLinearProvider()
	p.mu.Lock()
	p.httpClient = cli
	p.cachedEndpoint = mock.URL()
	p.cachedToken = "lin_api_x"
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		p.httpClient = nil
		p.cachedEndpoint = ""
		p.cachedToken = ""
		p.mu.Unlock()
	})

	res, _, _ := b.linearGetTool(context.Background(), &mcp.CallToolRequest{}, linearGetInput{Number: 9})
	if res.IsError {
		t.Fatalf("get errored: %s", textContent(res))
	}
	// Expect text + 1 image content block.
	if len(res.Content) != 2 {
		t.Fatalf("content=%d blocks, want 2 (%+v)", len(res.Content), res.Content)
	}
	img, ok := res.Content[1].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content[1] is not ImageContent: %+v", res.Content[1])
	}
	if string(img.Data) != "IMG-BYTES" || img.MIMEType != "image/png" {
		t.Errorf("image block=%+v", img)
	}
}

// hostRewriteTransport rewrites the request URL host to a custom
// host:port pair before dispatching. Used in TestLinearGetTool_InlinesImagesFromDescription
// to redirect uploads.linear.app to an httptest server without
// touching DNS.
type hostRewriteTransport struct {
	from string
	to   string
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Host, t.from) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = t.to
		clone.Host = t.to
		return http.DefaultTransport.RoundTrip(clone)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// Compile-time guard: catch accidental unused imports if tests get trimmed.
var (
	_ = json.Marshal
	_ = http.StatusOK
)
