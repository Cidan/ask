package tools

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestParseManualAuth(t *testing.T) {
	const authURL = "https://as.example/authorize?state=OUTGOING&client_id=x"
	cases := []struct {
		name      string
		input     string
		wantCode  string
		wantState string
		wantIss   string
		wantErr   bool
	}{
		{"full redirect url", "http://127.0.0.1:5000/callback?code=AC&state=ST&iss=https://as", "AC", "ST", "https://as", false},
		{"query only", "?code=AC2&state=ST2", "AC2", "ST2", "", false},
		{"bare code recovers state from auth url", "BARECODE", "BARECODE", "OUTGOING", "", false},
		{"whitespace trimmed", "  http://127.0.0.1/callback?code=AC3&state=ST3  ", "AC3", "ST3", "", false},
		{"empty", "", "", "", "", true},
		{"error redirect", "http://127.0.0.1/callback?error=access_denied", "", "", "", true},
		{"url without code", "http://127.0.0.1/callback?state=only", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseManualAuth(tc.input, authURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Code != tc.wantCode || res.State != tc.wantState || res.Iss != tc.wantIss {
				t.Errorf("got %+v want code=%q state=%q iss=%q", res, tc.wantCode, tc.wantState, tc.wantIss)
			}
		})
	}
}

func stubNoBrowser(t *testing.T) {
	t.Helper()
	prev := MCPOAuthOpenBrowser
	MCPOAuthOpenBrowser = func(string) error { return nil }
	t.Cleanup(func() { MCPOAuthOpenBrowser = prev })
}

func TestLazyOAuthCallback_FetchUsesPastedRedirect(t *testing.T) {
	stubNoBrowser(t)
	cb, err := newLazyOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	cb.prompter = func(ctx context.Context, authURL string) (string, error) {
		return "http://127.0.0.1:9/callback?code=PASTED&state=PS", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cb.Fetch(ctx, &auth.AuthorizationArgs{URL: "https://as/authorize?state=IGNORED"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "PASTED" || res.State != "PS" {
		t.Errorf("pasted redirect must win: %+v", res)
	}
}

func TestLazyOAuthCallback_FetchBareCodeRecoversState(t *testing.T) {
	stubNoBrowser(t)
	cb, err := newLazyOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	cb.prompter = func(ctx context.Context, authURL string) (string, error) {
		return "JUSTACODE", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cb.Fetch(ctx, &auth.AuthorizationArgs{URL: "https://as/authorize?state=RECOVERED"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "JUSTACODE" || res.State != "RECOVERED" {
		t.Errorf("bare code must recover state from the auth URL: %+v", res)
	}
}

func TestLazyOAuthCallback_FetchPrompterCancel(t *testing.T) {
	stubNoBrowser(t)
	cb, err := newLazyOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("user cancelled")
	cb.prompter = func(ctx context.Context, authURL string) (string, error) {
		return "", sentinel
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cb.Fetch(ctx, &auth.AuthorizationArgs{URL: "https://as/authorize?state=x"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("prompter error must propagate: %v", err)
	}
}

// TestLazyOAuthCallback_FetchLoopbackWinsOverPrompt proves the loopback
// callback (local browser or ssh -L) still completes the flow even while the
// paste prompt is open: the prompter blocks, the loopback delivers the code.
func TestLazyOAuthCallback_FetchLoopbackWinsOverPrompt(t *testing.T) {
	stubNoBrowser(t)
	cb, err := newLazyOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	cb.prompter = func(ctx context.Context, authURL string) (string, error) {
		// Simulate the browser redirect hitting the bound loopback, then block
		// as a real paste prompt would until the flow cancels it.
		go func() {
			for i := 0; i < 50; i++ {
				resp, err := http.Get(cb.url + "?code=LOOP&state=LS")
				if err == nil {
					_ = resp.Body.Close()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cb.Fetch(ctx, &auth.AuthorizationArgs{URL: "https://as/authorize?state=x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "LOOP" || res.State != "LS" {
		t.Errorf("loopback callback must win: %+v", res)
	}
}
