package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Linear stores screenshots and other inline media on a private
// uploads.linear.app CDN that requires the same Authorization header
// the GraphQL API uses. The chat agent therefore can't fetch those
// URLs on its own — we have to do it server-side and pack the bytes
// into MCP ImageContent blocks the model can actually read.
//
// Caps below keep one issue from blowing the context window. The
// numbers are conservative; a single descriptive screenshot already
// dominates a tool result, so 4 / 5 MB is plenty in practice and
// anything bigger should stay as a URL in the markdown body.
//
// Token-bearing fetches are restricted to the `uploads.linear.app`
// host (see [isLinearImageURL]) so the Linear API key never leaks
// to third-party CDNs that descriptions might reference.

const (
	linearImageHost          = "uploads.linear.app"
	linearImageMaxCount      = 4
	linearImageMaxBytesEach  = 4 * 1024 * 1024 // 4 MB cap per image
	linearImageMaxBytesTotal = 8 * 1024 * 1024 // 8 MB cumulative cap
	linearImageFetchTimeout  = 15 * time.Second
	// linearImageMaxAttempts bounds total fetch attempts so an issue
	// littered with broken or oversized image links can't drag the
	// tool call out to minutes. The success cap above only stops once
	// linearImageMaxCount images have been *kept*; without a separate
	// attempt cap, dozens of failing refs would each consume up to
	// linearImageFetchTimeout. Set generously above the success cap
	// so a few transient failures still let downstream good URLs
	// through.
	linearImageMaxAttempts = linearImageMaxCount * 2
)

// linearImageMarkdownRe matches markdown image syntax `![alt](url)`.
// The URL group is bounded by either a closing paren or whitespace so
// titles like `![](url "caption")` strip cleanly.
var linearImageMarkdownRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)`)

// linearImageRef is one image URL surfaced from markdown plus a tag
// describing where it came from. The tag isn't currently surfaced to
// the agent but makes debug logging readable.
type linearImageRef struct {
	URL    string
	Source string
}

// linearExtractImageURLs walks description + each comment body in
// order, returning every Linear-hosted image URL exactly once.
// Non-Linear hosts are dropped silently so we never round-trip a
// bearer token to an unknown CDN.
func linearExtractImageURLs(description string, comments []string) []linearImageRef {
	seen := map[string]bool{}
	var out []linearImageRef
	scan := func(body, source string) {
		for _, m := range linearImageMarkdownRe.FindAllStringSubmatch(body, -1) {
			raw := strings.TrimSpace(m[1])
			if !isLinearImageURL(raw) {
				continue
			}
			if seen[raw] {
				continue
			}
			seen[raw] = true
			out = append(out, linearImageRef{URL: raw, Source: source})
		}
	}
	scan(description, "description")
	for _, c := range comments {
		scan(c, "comment")
	}
	return out
}

// isLinearImageURL accepts only https URLs whose host matches
// uploads.linear.app exactly (case-insensitive). External images
// pasted into a Linear description are deliberately ignored.
//
// HTTPS is required because the auth round-tripper attaches the
// Linear API key on every fetch; a plaintext http:// URL would
// leak the key to anyone observing the network.
func isLinearImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, linearImageHost)
}

// linearFetchedImage bundles the bytes + MIME of one inlined image.
type linearFetchedImage struct {
	URL      string
	Data     []byte
	MIMEType string
}

// linearImageFetcher is the indirection seam used by handlers and
// tests. Production wires this to (*linearIssueProvider).fetchImage,
// reusing the cached http.Client + auth round-tripper so token
// rotation flows through one place.
type linearImageFetcher interface {
	fetchImage(ctx context.Context, cfg linearMCPConfig, rawURL string) (data []byte, mimeType string, err error)
}

// linearGatherImages walks refs in order, stopping when either the
// success-count cap or the total-attempts cap is exhausted. Per-image
// failures are counted into dropped and otherwise swallowed so a
// broken CDN entry can't take down the whole tool call.
//
// The attempt cap is what keeps a stale issue with dozens of broken
// upload links from dragging the tool call into the minutes — without
// it, len(images) wouldn't advance and the loop would issue one
// linearImageFetchTimeout-bounded request per ref.
func linearGatherImages(ctx context.Context, f linearImageFetcher, cfg linearMCPConfig, refs []linearImageRef) (images []linearFetchedImage, dropped int) {
	if f == nil || len(refs) == 0 {
		return nil, 0
	}
	total := 0
	attempts := 0
	for i, r := range refs {
		if len(images) >= linearImageMaxCount || attempts >= linearImageMaxAttempts {
			// Caps reached. Count every remaining ref as dropped
			// in one shot and exit, so we don't keep iterating
			// the list to no effect.
			dropped += len(refs) - i
			break
		}
		attempts++
		ictx, cancel := context.WithTimeout(ctx, linearImageFetchTimeout)
		body, mime, err := f.fetchImage(ictx, cfg, r.URL)
		cancel()
		if err != nil {
			dropped++
			continue
		}
		if len(body) == 0 || len(body) > linearImageMaxBytesEach {
			dropped++
			continue
		}
		if total+len(body) > linearImageMaxBytesTotal {
			dropped++
			continue
		}
		total += len(body)
		images = append(images, linearFetchedImage{
			URL:      r.URL,
			Data:     body,
			MIMEType: mime,
		})
	}
	return images, dropped
}

// linearBuildIssueResult assembles the MCP CallToolResult for any
// handler that returns a linearIssueDetailView. The text block holds
// the marshalled JSON payload exactly as before; image blocks are
// appended for each Linear-hosted image referenced by description or
// comments. A trailing text note records anything that overflowed
// the cap so the agent doesn't silently lose images.
func linearBuildIssueResult(ctx context.Context, f linearImageFetcher, cfg linearMCPConfig, text, description string, comments []string) *mcp.CallToolResult {
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	refs := linearExtractImageURLs(description, comments)
	if len(refs) == 0 {
		return &mcp.CallToolResult{Content: content}
	}
	images, dropped := linearGatherImages(ctx, f, cfg, refs)
	for _, img := range images {
		content = append(content, &mcp.ImageContent{
			Data:     img.Data,
			MIMEType: img.MIMEType,
		})
	}
	if dropped > 0 {
		content = append(content, &mcp.TextContent{
			Text: fmt.Sprintf(
				"(linear: %d image%s omitted — per-issue cap is %d images / %d MB)",
				dropped, pluralS(dropped), linearImageMaxCount, linearImageMaxBytesTotal/(1024*1024),
			),
		})
	}
	return &mcp.CallToolResult{Content: content}
}

// linearCommentBodies pulls just the body field out of a slice of
// issue comments. Tiny helper, but keeps the call sites in
// mcp_linear.go from re-implementing the loop twice.
func linearCommentBodies(cs []issueComment) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.body)
	}
	return out
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fetchImage implements linearImageFetcher on linearIssueProvider.
// Reuses the same cached transport that callGraphQL primes so the
// Authorization round-tripper attaches the personal API key without
// duplicating the cache invalidation logic on token rotation.
//
// SECURITY: two host-gate layers protect the API key.
//  1. The pre-flight check on rawURL refuses anything that isn't an
//     https://uploads.linear.app URL outright.
//  2. The image client wraps the shared transport with a CheckRedirect
//     that re-runs the same gate on every hop. A 30x from
//     uploads.linear.app to a third-party host would otherwise let
//     net/http follow the redirect with the auth header still attached;
//     re-gating fails the request closed instead.
func (p *linearIssueProvider) fetchImage(ctx context.Context, cfg linearMCPConfig, rawURL string) ([]byte, string, error) {
	if !isLinearImageURL(rawURL) {
		return nil, "", fmt.Errorf("linear image: refusing to fetch non-linear host %q", rawURL)
	}
	endpoint := linearGraphQLEndpointOrDefault(cfg)
	p.mu.Lock()
	if p.httpClient == nil || p.cachedEndpoint != endpoint || p.cachedToken != cfg.Token {
		p.httpClient = &http.Client{
			Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: cfg.Token},
			Timeout:   linearGraphQLCallTimeout,
		}
		p.cachedEndpoint = endpoint
		p.cachedToken = cfg.Token
		p.statesMu.Lock()
		p.statesCache = nil
		p.statesMu.Unlock()
		p.teamIDMu.Lock()
		p.teamIDCache = nil
		p.teamIDMu.Unlock()
	}
	transport := p.httpClient.Transport
	p.mu.Unlock()

	imageClient := &http.Client{
		Transport:     transport,
		Timeout:       linearGraphQLCallTimeout,
		CheckRedirect: linearImageCheckRedirect,
	}
	return linearFetchImageOverHTTP(ctx, imageClient, rawURL)
}

// linearImageCheckRedirect re-runs isLinearImageURL against every
// redirect target so the auth round-tripper cannot leak the API key
// to a non-Linear host that uploads.linear.app might redirect to.
// Also caps redirect chains so a misbehaving CDN can't loop us.
func linearImageCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("linear image: too many redirects")
	}
	if !isLinearImageURL(req.URL.String()) {
		return fmt.Errorf("linear image: blocked redirect to %q (host gate)", req.URL.Host)
	}
	return nil
}

// linearFetchImageOverHTTP is the pure HTTP+parse step behind
// fetchImage. Pulled out so tests can drive it against an httptest
// server without going through the host gate — host filtering is the
// caller's responsibility.
//
// Body reads are capped at linearImageMaxBytesEach+1 so an oversized
// asset surfaces as an explicit cap-exceeded drop in linearGatherImages
// rather than exhausting memory.
func linearFetchImageOverHTTP(ctx context.Context, cli *http.Client, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	// The shared round-tripper defaults Content-Type to application/json
	// for unset headers. A GET has no body, so pre-set Accept and a
	// benign Content-Type rather than letting the JSON default leak in.
	req.Header.Set("Accept", "image/*")
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := cli.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	// Reject 3xx as well as 4xx/5xx: with CheckRedirect refusing
	// off-host hops, an in-host 30x still indicates the request
	// didn't actually produce image bytes. Anything other than 200
	// (e.g. 204, 206) likewise has no usable body for our cap+sniff
	// logic, so fail closed.
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("linear image: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, linearImageMaxBytesEach+1))
	if err != nil {
		return nil, "", err
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "" || strings.EqualFold(mime, "application/octet-stream") {
		mime = http.DetectContentType(body)
		if i := strings.IndexByte(mime, ';'); i >= 0 {
			mime = strings.TrimSpace(mime[:i])
		}
	}
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return nil, "", fmt.Errorf("linear image: non-image content type %q", mime)
	}
	return body, mime, nil
}
