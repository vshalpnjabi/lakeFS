package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/treeverse/lakefs/pkg/api/apigen"
	"github.com/treeverse/lakefs/pkg/uri"
)

// tagCreateFake is a small lakeFS stand-in for tag create --force.
// Relative refs like "v1~1" resolve only while that tag exists. After the
// tag is deleted they 404, matching the server.
type tagCreateFake struct {
	mu     sync.Mutex
	tags   map[string]string
	commit map[string]apigen.Commit
	// relative maps a typed ref (e.g. "v1~1") to the tag it depends on and the parent commit.
	relative map[string]struct{ tag, commit string }
	// rejectRef, if set, makes CreateTag fail when Ref equals this value (hook-style reject).
	rejectRef string
	creates   []apigen.TagCreation
}

func (f *tagCreateFake) resolve(ref string) (apigen.Commit, bool) {
	if c, ok := f.commit[ref]; ok {
		return c, true
	}
	rel, ok := f.relative[ref]
	if !ok {
		return apigen.Commit{}, false
	}
	if _, exists := f.tags[rel.tag]; !exists {
		return apigen.Commit{}, false
	}
	c, ok := f.commit[rel.commit]
	return c, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *tagCreateFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.Contains(p, "/commits/"):
		ref := path.Base(p)
		c, ok := f.resolve(ref)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, c)

	case r.Method == http.MethodGet && strings.Contains(p, "/tags/"):
		tag := path.Base(p)
		commitID, ok := f.tags[tag]
		if !ok {
			writeAPIError(w, http.StatusNotFound, "tag not found")
			return
		}
		writeJSON(w, http.StatusOK, apigen.Ref{Id: tag, CommitId: commitID})

	case r.Method == http.MethodDelete && strings.Contains(p, "/tags/"):
		tag := path.Base(p)
		if _, ok := f.tags[tag]; !ok {
			writeAPIError(w, http.StatusNotFound, "tag not found")
			return
		}
		delete(f.tags, tag)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/tags"):
		var body apigen.TagCreation
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		f.creates = append(f.creates, body)
		c, ok := f.resolve(body.Ref)
		if f.rejectRef != "" && ((ok && c.Id == f.rejectRef) || body.Ref == f.rejectRef) {
			writeAPIError(w, http.StatusForbidden, "pre-create-tag hook aborted, run id 'run1' hook 'reject': tags are frozen")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		if _, exists := f.tags[body.Id]; exists {
			writeAPIError(w, http.StatusConflict, "tag already exists")
			return
		}
		f.tags[body.Id] = c.Id
		writeJSON(w, http.StatusCreated, apigen.Ref{Id: body.Id, CommitId: c.Id})

	default:
		writeAPIError(w, http.StatusNotFound, "unhandled "+r.Method+" "+p)
	}
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apigen.Error{Message: msg})
}

func TestCreateTagForceRelativeRefUsesResolvedCommit(t *testing.T) {
	t.Parallel()
	const (
		repo         = "rep"
		oldCommit    = "bbb222bbb222bbb222bbb222bbb222bb"
		parentCommit = "aaa111aaa111aaa111aaa111aaa111aa"
	)
	fake := &tagCreateFake{
		tags: map[string]string{"v1": oldCommit},
		commit: map[string]apigen.Commit{
			oldCommit:    {Id: oldCommit, Message: "two", Committer: "u", MetaRangeId: "mr2"},
			parentCommit: {Id: parentCommit, Message: "one", Committer: "u", MetaRangeId: "mr1"},
		},
		relative: map[string]struct{ tag, commit string }{
			"v1~1": {tag: "v1", commit: parentCommit},
		},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client := getTestClient(t, server.URL)

	got, err := createTag(t.Context(), client, &uri.URI{Repository: repo, Ref: "v1"}, &uri.URI{Repository: repo, Ref: "v1~1"}, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, parentCommit, got.CommitId)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, parentCommit, fake.tags["v1"], "tag should move to the already-resolved parent")
	require.NotEmpty(t, fake.creates)
	require.Equal(t, parentCommit, fake.creates[0].Ref, "CreateTag must use the resolved commit id, not v1~1")
}

func TestCreateTagForceRestoresTagWhenCreateFails(t *testing.T) {
	t.Parallel()
	const (
		repo      = "rep"
		oldCommit = "bbb222bbb222bbb222bbb222bbb222bb"
		newCommit = "ccc333ccc333ccc333ccc333ccc333cc"
	)
	fake := &tagCreateFake{
		tags: map[string]string{"v2": oldCommit},
		commit: map[string]apigen.Commit{
			oldCommit: {Id: oldCommit, Message: "two", Committer: "u", MetaRangeId: "mr2"},
			newCommit: {Id: newCommit, Message: "hook", Committer: "u", MetaRangeId: "mr3"},
			"main":    {Id: newCommit, Message: "hook", Committer: "u", MetaRangeId: "mr3"},
		},
		rejectRef: newCommit,
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client := getTestClient(t, server.URL)

	got, err := createTag(t.Context(), client, &uri.URI{Repository: repo, Ref: "v2"}, &uri.URI{Repository: repo, Ref: "main"}, true)
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "tags are frozen")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, oldCommit, fake.tags["v2"], "failed --force must leave the existing tag in place")
}
