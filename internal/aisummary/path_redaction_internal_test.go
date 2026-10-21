package aisummary

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/redact"
)

const pathSecret, pathProfile = "SECRETVALUE", "names"

func pathRedactionSession(t *testing.T, endpoint string) *Session {
	t.Helper()

	profile := testProfile(endpoint)
	profile.Redact = []redact.Rule{{Pattern: pathSecret, Replacement: "[OCULTO]"}}

	var selection config.Repository

	selection.IACommit = []string{pathProfile}
	selection.IAInclude = []string{"/token-*"}

	session, err := New(map[string]config.AIProfile{pathProfile: profile}).Session(&selection)
	if err != nil {
		t.Fatal(err)
	}

	return session
}

func stagedSecretName(t *testing.T) (*gitrepo.Repository, map[string]string) {
	t.Helper()
	repository := &gitrepo.Repository{Path: t.TempDir(), Host: "ai-test", Timeout: 3 * time.Second}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(repository.Path, "token-"+pathSecret+".conf")

	err = os.WriteFile(path, []byte("VALUE=one\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.Commit(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte("VALUE=two\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = repository.Stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := repository.StagedVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return repository, versions
}

func TestCommitDiffMasksPathNames(t *testing.T) {
	t.Parallel()
	repo, versions := stagedSecretName(t)
	bodies := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}

		bodies <- string(body)

		writeReply(t, writer, openAIReply)
	}))
	defer server.Close()

	text, status := pathRedactionSession(t, server.URL).Summarize(t.Context(), repo, versions)
	if text == "" || status != "summarized:"+pathProfile {
		t.Fatal(text, status)
	}

	body := <-bodies
	if strings.Contains(body, pathSecret) || !strings.Contains(body, "token-[OCULTO].conf") {
		t.Fatal("commit diff header sent an unmasked path name", body)
	}
}

func TestUnmaskablePathNameIsOmitted(t *testing.T) {
	t.Parallel()
	repo, staged := stagedFixture(t)

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeReply(t, writer, openAIReply)
	}))
	defer server.Close()

	// One match past the redaction limit rejects the name while its patch still fits max_diff_bytes.
	versions := map[string]string{"/token-" + strings.Repeat(pathSecret, 1025): staged["/config"]}

	text, status := pathRedactionSession(t, server.URL).Summarize(t.Context(), repo, versions)
	if status != "omitted" || !strings.Contains(text, "1 archivos autorizados omitidos") || requests.Load() != 0 {
		t.Fatal("unmaskable path name was not omitted", text, status, requests.Load())
	}
}
