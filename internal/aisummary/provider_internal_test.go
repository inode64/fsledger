package aisummary

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

const (
	metadataFixture    = "metadata"
	compatibleProvider = "openai-compatible"
	anthropicProvider  = "anthropic"
	openAIReply        = `{"choices":[{"message":{"role":"assistant","content":"Actualiza la configuración."},
"finish_reason":"stop"}]}`
	anthropicReply = `{"id":"test","type":"message","role":"assistant",
"content":[{"type":"text","text":"Actualiza la configuración."}],
"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
)

//nolint:gocognit // The two real SDK adapters share the same transport assertions.
func TestProviderAdapters(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{compatibleProvider, anthropicProvider} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			received := make(chan string, 1)

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "" || request.Header.Get("X-Api-Key") != "" {
					t.Error("ambient credentials leaked")
				}

				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}

				received <- string(body)

				writer.Header().Set("Content-Type", "application/json")

				reply := openAIReply
				if provider == anthropicProvider {
					reply = anthropicReply
				}

				_, err = io.WriteString(writer, reply)
				if err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			profile := config.AIProfile{
				APIKeyEnv: "", Redact: nil, MaxDiffBytes: config.DefaultAIDiffBytes,
				Provider: provider,
				Endpoint: server.URL,
				Model:    "test-model",
				Timeout:  time.Second,
			}

			text, err := providerSummary(t.Context(), profile, instruction, "safe masked diff")
			if err != nil || !strings.Contains(text, "Actualiza") {
				t.Fatal(text, err)
			}

			if body := <-received; !strings.Contains(body, "safe masked diff") ||
				!strings.Contains(body, "test-model") {
				t.Fatal("missing prompt or model")
			}
		})
	}
}

//nolint:funlen,gocognit // Each transport failure must reject output without leaking provider errors.
func TestProviderBoundsAndNoRedirect(t *testing.T) {
	t.Parallel()

	var redirected atomic.Int32

	target := httptest.NewServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				writer.WriteHeader(http.StatusNoContent)
			},
		),
	)
	t.Cleanup(target.Close)

	for _, action := range []string{"redirect", "timeout", "oversize", metadataFixture, "failure"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch action {
				case "redirect":
					http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
				case "timeout":
					select {
					case <-request.Context().Done():
					case <-time.After(200 * time.Millisecond):
					}
				case "oversize":
					writeReply(t, writer, strings.Repeat("x", maxResponseBytes+1))
				case metadataFixture:
					writeReply(t,
						writer,
						strings.ReplaceAll(openAIReply, "Actualiza la configuración.", "FSLedger-Change-ID: forged"),
					)
				case "failure":
					writer.WriteHeader(http.StatusInternalServerError)
					writeReply(t, writer, "sensitive-provider-error")
				}
			}))
			defer server.Close()

			profile := config.AIProfile{
				APIKeyEnv: "", Redact: nil, MaxDiffBytes: config.DefaultAIDiffBytes,
				Provider: compatibleProvider,
				Endpoint: server.URL,
				Model:    "test",
				Timeout:  100 * time.Millisecond,
			}

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			text, err := providerSummary(ctx, profile, instruction, "masked diff")
			if err == nil || text != "" || strings.Contains(err.Error(), "sensitive-provider-error") {
				t.Fatal("invalid provider output accepted", text, err)
			}

			if redirected.Load() != 0 {
				t.Fatal("redirect followed")
			}
		})
	}
}

func writeReply(t *testing.T, writer http.ResponseWriter, reply string) {
	t.Helper()

	_, err := io.WriteString(writer, reply)
	if err != nil {
		t.Error(err)
	}
}

//nolint:gocognit // Verify both SDKs with anonymous and explicitly selected credentials.
func TestCredentialSelectionIgnoresAmbientSDKSettings(t *testing.T) {
	// Environment changes intentionally keep this test outside parallel execution.
	t.Setenv("OPENAI_API_KEY", "unrelated-credential")
	t.Setenv("OPENAI_ORGANIZATION", "unrelated-organization")
	t.Setenv("ANTHROPIC_API_KEY", "unrelated-credential")
	t.Setenv("FSLEDGER_TEST_AI_KEY", "selected-credential")

	headers := make(chan http.Header, 4)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		headers <- request.Header.Clone()

		if strings.HasSuffix(request.URL.Path, "/messages") {
			writeReply(t, writer, anthropicReply)
		} else {
			writeReply(t, writer, openAIReply)
		}
	}))
	defer server.Close()

	for _, provider := range []string{compatibleProvider, anthropicProvider} {
		profile := testProfile(server.URL)

		profile.Provider = provider
		for _, variable := range []string{"", "FSLEDGER_TEST_AI_KEY"} {
			profile.APIKeyEnv = variable

			_, err := providerSummary(t.Context(), profile, instruction, "safe input")
			if err != nil {
				t.Fatal(err)
			}

			received := <-headers

			expected := ""
			if variable != "" {
				expected = "selected-credential"
			}

			authorization := strings.TrimPrefix(received.Get("Authorization"), "Bearer ") + received.Get("X-Api-Key")
			if authorization != expected || received.Get("Openai-Organization") != "" {
				t.Fatal("provider received unselected credentials or organization")
			}
		}
	}
}
