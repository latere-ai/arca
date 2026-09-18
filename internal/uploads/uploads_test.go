// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// errOutage is a store that answers nothing at all.
var errOutage = errors.New("the store is unreachable")

func TestASessionOpensUploadsAndCompletes(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	if session.PartSize != PartSize || session.PartCount != 2 || len(session.PartURLs) != 2 {
		t.Fatalf("the session is %+v", session)
	}
	if session.Owner != h.owner || session.Path != "files/video/keynote.mp4" {
		t.Fatalf("the session names %q at %q", session.Owner, session.Path)
	}
	if _, err := time.Parse(time.RFC3339, session.ExpiresAt); err != nil {
		t.Fatalf("the session expires at %q: %v", session.ExpiresAt, err)
	}
	// The declared bytes count from the moment the session opens.
	if held := h.store.usage[h.owner]; held != 20<<20 {
		t.Fatalf("an open session leaves the space holding %d bytes", held)
	}

	first := h.upload(t, session, 1, "the first part")
	second := h.upload(t, session, 2, " and the second")
	w := h.finish(t, session, []string{first, second})
	if w.Code != http.StatusCreated {
		t.Fatalf("the completion answered %d: %s", w.Code, w.Body)
	}
	var out files.Object
	decode(t, w, &out)
	if out.Path != "files/video/keynote.mp4" || out.Size != int64(len("the first part and the second")) {
		t.Fatalf("the completion answered %+v", out)
	}
	// The checksum of a multipart object is the store's composite label and
	// not a digest of the bytes, and the row says which it is.
	if out.ChecksumKind != string(object.ChecksumETag) || out.Checksum == "" {
		t.Fatalf("the completion answered the checksum %q of kind %q", out.Checksum, out.ChecksumKind)
	}
	if got := w.Header().Get(api.HeaderETag); got != `"`+out.Checksum+`"` {
		t.Errorf("the ETag is %q", got)
	}
	// The space is charged the assembled size and not the declared one, and
	// the session is gone.
	if held := h.store.usage[h.owner]; held != out.Size {
		t.Fatalf("the space holds %d bytes of the %d the object weighs", held, out.Size)
	}
	if len(h.store.sessions) != 0 {
		t.Errorf("the completion left %d sessions", len(h.store.sessions))
	}
	if len(h.store.events) != 1 || h.store.events[0].Action != files.EventPut {
		t.Errorf("the log holds %v", h.store.events)
	}
}

func TestASizeNoRouteServesIsRefusedAndOpensNoMultipart(t *testing.T) {
	h := newHarness(t)
	for body, want := range map[string]string{
		`{"owner":"me","path":"files/a.mp4","size":0}`:             api.CodeInvalidField,
		`{"owner":"me","path":"files/a.mp4","size":-1}`:            api.CodeInvalidField,
		`{"owner":"me","path":"files/a.mp4","size":6000000000}`:    api.CodeObjectTooLarge,
		`{"owner":"me","path":"nowhere/a.mp4","size":1024}`:        api.CodeUnknownPlane,
		`{"owner":"me","path":"workspaces/a.mp4","size":1024}`:     api.CodeInvalidPath,
		`{"owner":"me","path":"files/a.mp4","size":1024,"z":true}`: api.CodeUnknownField,
	} {
		if got := code(t, h.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != want {
			t.Errorf("%s is %q, want %q", body, got, want)
		}
	}
	if h.bucket.Calls(blob.MethodCreateMultipart) != 0 {
		t.Error("a refused session opened a multipart")
	}

	// The part cap is a guard against a client asking for a million URLs,
	// and it is read before anything opens.
	big := newHarness(t, func(o *Options) { o.Config.MaxUploadBytes = 1 << 50 })
	body := `{"owner":"me","path":"files/a.mp4","size":` + itoa(MaxParts*PartSize+1) + `}`
	if got := code(t, big.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != api.CodeTooManyParts {
		t.Errorf("a size above the part cap is %q", got)
	}
	if big.bucket.Calls(blob.MethodCreateMultipart) != 0 {
		t.Error("a session above the part cap opened a multipart")
	}
}

func TestASessionAtOrBelowTheBoundaryIsAccepted(t *testing.T) {
	h := newHarness(t)
	// A client that already knows it wants resumable parts should not be
	// argued with, so the boundary is what a put refuses above and not what
	// a session refuses below.
	session := h.open(t, "files/small.bin", 1024)
	if session.PartCount != 1 {
		t.Fatalf("a session of one part answered %d", session.PartCount)
	}
}

func TestACompletionWithAWrongPartLabelLeavesNoRow(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	h.upload(t, session, 1, "the first part")
	w := h.finish(t, session, []string{"not-the-label-the-store-answered"})
	if got := code(t, w); got != api.CodeBadRequest {
		t.Fatalf("a completion with a wrong label is %q", got)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/video/keynote.mp4"); err == nil {
		t.Error("a failed assembly left a row")
	}
}

func TestACompletionNamesItsParts(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	for body, want := range map[string]string{
		`{"parts":[]}`:                         api.CodeMissingField,
		`{"parts":[{"n":0,"etag":"x"}]}`:       api.CodeInvalidField,
		`{"parts":[{"n":1,"etag":""}]}`:        api.CodeInvalidField,
		`{"parts":[{"n":1,"label":"x"}]}`:      api.CodeUnknownField,
		`{"parts":[{"n":1,"etag":"x"}],"z":1}`: api.CodeUnknownField,
	} {
		w := h.call(t, http.MethodPost, "/v1/uploads/"+session.ID+"/complete", strings.NewReader(body))
		if got := code(t, w); got != want {
			t.Errorf("%s is %q, want %q", body, got, want)
		}
	}
}

func TestACompletionIsResumableAfterTheRowWriteFailed(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	first := h.upload(t, session, 1, "the first part")

	// The first attempt assembles the object and fails at the database.
	h.store.failTx = errOutage
	if w := h.finish(t, session, []string{first}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a completion whose row write failed answered %d: %s", w.Code, w.Body)
	}
	if len(h.store.sessions) != 1 {
		t.Fatal("a completion that failed at the database dropped its session")
	}

	// The retry finds the upload id gone and the object there, so it resumes
	// from the row write rather than re-uploading a part.
	h.store.failTx = nil
	w := h.finish(t, session, []string{first})
	if w.Code != http.StatusCreated {
		t.Fatalf("the retry answered %d: %s", w.Code, w.Body)
	}
	var out files.Object
	decode(t, w, &out)
	if out.Size != int64(len("the first part")) {
		t.Fatalf("the retry answered %+v", out)
	}
}

func TestACompletionPastTheAnswersLimitIsRefusedAndTheObjectGoes(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	first := h.upload(t, session, 1, strings.Repeat("x", 4096))
	// The declared size was a promise and the head is the fact, so the
	// answer's limit is read against what the store holds.
	h.endpoint.SetRules(stub.Rule{
		Subject: "*", Action: "*", Resource: "*", Allow: true,
		Limits: map[string]any{"quota_bytes": 2048},
	})
	w := h.finish(t, session, []string{first})
	if got := code(t, w); got != api.CodeQuotaExceeded {
		t.Fatalf("a completion past the answer's limit is %q", got)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/video/keynote.mp4"); err == nil {
		t.Error("a refused completion left a row")
	}
	if len(h.objects.Keys()) != 0 {
		t.Errorf("a refused completion left %d objects", len(h.objects.Keys()))
	}
	if len(h.store.sessions) != 0 {
		t.Error("a refused completion left its session")
	}
	if held := h.store.usage[h.owner]; held != 0 {
		t.Errorf("a refused completion left the space holding %d bytes", held)
	}
}

func TestTheConditionalWriteContractIsTheSameAtCompletionAsAtAPut(t *testing.T) {
	h := newHarness(t)
	// A create-only completion onto a path a live row holds is refused
	// before anything is assembled.
	taken := h.open(t, "files/plan.md", 1024)
	first := h.upload(t, taken, 1, "first")
	if w := h.finish(t, taken, []string{first}); w.Code != http.StatusCreated {
		t.Fatalf("the first completion answered %d: %s", w.Code, w.Body)
	}

	second := h.open(t, "files/plan.md", 1024)
	part := h.upload(t, second, 1, "second")
	before := h.bucket.Calls(blob.MethodCompleteMultipart)
	w := h.finish(t, second, []string{part}, api.HeaderIfNoneMatch, "*")
	if got := code(t, w); got != api.CodePreconditionFailed {
		t.Fatalf("a create-only completion onto a live path is %q", got)
	}
	if h.bucket.Calls(blob.MethodCompleteMultipart) != before {
		t.Error("a doomed completion assembled its parts anyway")
	}
	if len(h.store.sessions) != 0 {
		t.Error("a refused completion left its session")
	}

	// A completion with a stale If-Match is refused the same way, and one
	// with neither header succeeds.
	stale := h.open(t, "files/plan.md", 1024)
	part = h.upload(t, stale, 1, "third")
	if got := code(t, h.finish(t, stale, []string{part}, api.HeaderIfMatch, `"nothing"`)); got != api.CodePreconditionFailed {
		t.Error("a stale If-Match at completion was accepted")
	}
	plain := h.open(t, "files/plan.md", 1024)
	part = h.upload(t, plain, 1, "fourth")
	if w := h.finish(t, plain, []string{part}); w.Code != http.StatusOK {
		t.Fatalf("an overwrite through a session answered %d: %s", w.Code, w.Body)
	}
	// An overwrite through a session captures a version, and the previous
	// object's bytes survive behind it.
	if len(h.store.versions) != 1 {
		t.Fatalf("an overwrite through a session left %d versions", len(h.store.versions))
	}
}

func TestAnAbortDiscardsThePartsAndTheRowAndTellsTheTruthWhenItCannot(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	h.upload(t, session, 1, "the first part")
	if w := h.call(t, http.MethodDelete, "/v1/uploads/"+session.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("the abort answered %d: %s", w.Code, w.Body)
	}
	if len(h.store.sessions) != 0 {
		t.Error("the abort left its session")
	}
	if held := h.store.usage[h.owner]; held != 0 {
		t.Errorf("the abort left the space holding %d bytes", held)
	}
	if got := code(t, h.call(t, http.MethodDelete, "/v1/uploads/"+session.ID, nil)); got != api.CodeNotFound {
		t.Errorf("an abort of a session that is gone is %q", got)
	}
	if got := code(t, h.call(t, http.MethodDelete, "/v1/uploads/not-an-identifier", nil)); got != api.CodeNotFound {
		t.Errorf("an abort of an id that is not one is %q", got)
	}

	// A store that will not take the parts back keeps the row, because the
	// row is the only durable pointer to them, and the caller is told the
	// outage rather than a false success.
	stubborn := newHarness(t)
	kept := stubborn.open(t, "files/video/keynote.mp4", 20<<20)
	stubborn.bucket.FailNth(blob.MethodAbortMultipart, 1, errOutage)
	w := stubborn.call(t, http.MethodDelete, "/v1/uploads/"+kept.ID, nil)
	if got := code(t, w); got != api.CodeStorageUnavailable {
		t.Fatalf("an abort the store refused is %q", got)
	}
	if len(stubborn.store.sessions) != 1 {
		t.Error("an abort the store refused dropped the row that points at the parts")
	}
}

func TestASessionIsInvisibleToEverySubjectTheAuthorizerRefuses(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 20<<20)
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "no rule allows it"})
	for _, c := range []struct{ method, target string }{
		{http.MethodDelete, "/v1/uploads/" + session.ID},
		{http.MethodPost, "/v1/uploads/" + session.ID + "/complete"},
	} {
		body := strings.NewReader(`{"parts":[{"n":1,"etag":"x"}]}`)
		w := h.call(t, c.method, c.target, body)
		if w.Code != http.StatusForbidden {
			t.Errorf("a refused %s answered %d: %s", c.method, w.Code, w.Body)
		}
	}
}

// TestASessionAnotherSubjectHoldsIsASessionThatIsNotThere is invariant 6 of
// spec 001 on the upload surface. A session id is a bearer-shaped string,
// and a caller that guesses one must not learn from the answer whether it
// named a real session. So a deny at lookup and an id nobody holds are one
// answer, developer detail included.
func TestASessionAnotherSubjectHoldsIsASessionThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	stranger := "https://issuer.example|someone-else"
	held, err := h.store.Insert2(t.Context(), nil, store.Session{
		Owner: stranger, Path: "files/video/keynote.mp4", ObjectID: object.NewID(),
		UploadID: "multipart-1", DeclaredSize: 20 << 20, ContentType: "video/mp4",
		CreatedBy: stranger, CreatedAt: h.clock, ExpiresAt: h.clock.Add(TTL),
	})
	if err != nil {
		t.Fatalf("seeding the stranger's session: %v", err)
	}
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "no rule allows it"})
	denied := h.call(t, http.MethodDelete, "/v1/uploads/"+held.ID, nil)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("a session another subject holds answered %d: %s", denied.Code, denied.Body)
	}
	gone := h.call(t, http.MethodDelete, "/v1/uploads/01JQZ0000000000000000000A", nil)
	if strip(denied.Body.String()) != strip(gone.Body.String()) {
		t.Errorf("a denied session answers\n %s\nand an unknown id answers\n %s", denied.Body, gone.Body)
	}
}

// strip renders a refusal without the one field two of them are allowed to
// differ in: the request id.
func strip(body string) string {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return body
	}
	if refusal, ok := envelope["error"].(map[string]any); ok {
		if details, ok := refusal["details"].(map[string]any); ok {
			delete(details, "request_id")
		}
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return string(out)
}

func TestEverySessionRouteAsksUploadWriteAndNothingElse(t *testing.T) {
	spec := routesOfSpec013(t)
	for _, row := range Table() {
		key := row.Method + " " + row.Path
		action, named := spec[key]
		if !named {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		if action != row.Action || row.Action != "upload.write" {
			t.Errorf("%s asks %q; spec 013's row says %q", key, row.Action, action)
		}
		if row.Summary == "" || row.Status == 0 {
			t.Errorf("%s is described as %+v", key, row)
		}
	}
	if len(Table()) != 3 {
		t.Errorf("this spec declares %d rows", len(Table()))
	}
}

func TestAStoreThatAnswersNothingIsAnOutageAndNeverAVerdict(t *testing.T) {
	// The multipart cannot be opened.
	opening := newHarness(t)
	opening.bucket.FailNth(blob.MethodCreateMultipart, 1, errOutage)
	body := `{"owner":"me","path":"files/a.mp4","size":1024}`
	if got := code(t, opening.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != api.CodeStorageUnavailable {
		t.Errorf("a store that would not open the upload is %q", got)
	}

	// The row cannot be written, so the multipart the create opened goes.
	writing := newHarness(t)
	writing.store.failWrite = errOutage
	if got := code(t, writing.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != api.CodeStorageUnavailable {
		t.Errorf("a store that would not write the session is %q", got)
	}
	if writing.bucket.Calls(blob.MethodAbortMultipart) != 1 {
		t.Error("a session whose row never committed left its multipart open")
	}

	// A part cannot be signed, so the session that was opened is closed.
	signing := newHarness(t)
	signing.bucket.FailNth(blob.MethodPresignPart, 1, errOutage)
	if got := code(t, signing.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != api.CodeStorageUnavailable {
		t.Errorf("a store that would not sign a part is %q", got)
	}
	if len(signing.store.sessions) != 0 {
		t.Error("a session whose parts could not be signed was left open")
	}

	// The ledger cannot be written.
	counting := newHarness(t)
	counting.ledger.refuse = errOutage
	if got := code(t, counting.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))); got != api.CodeStorageUnavailable {
		t.Errorf("a ledger that cannot be written is %q", got)
	}

	// The session cannot be read.
	reading := newHarness(t)
	session := reading.open(t, "files/a.mp4", 1024)
	first := reading.upload(t, session, 1, "x")
	reading.bucket.FailNth(blob.MethodHead, 1, errOutage)
	reading.bucket.FailNth(blob.MethodCompleteMultipart, 1, errOutage)
	if got := code(t, reading.finish(t, session, []string{first})); got != api.CodeBadRequest {
		t.Errorf("a completion the store would not assemble is %q", got)
	}
}

// TestTheDefaultsOfABuildWithNoQuerySetsBound: the node binds the ones over
// Postgres, and a session surface built with none still builds.
func TestTheDefaultsOfABuildWithNoQuerySetsBound(t *testing.T) {
	s := New(Options{DB: newMemory()})
	if s.sessions == nil || s.now == nil || s.content == nil {
		t.Fatal("a session surface built with no query sets bound none")
	}
	// A node that bound no registry records nothing, and every call site is
	// still one line rather than a branch (spec 018).
	if s.metrics == nil {
		t.Fatal("a session surface built with no recording surface bound none")
	}
	s.metrics.UploadSession(sessionCreated)
	s.metrics.UploadPart(partPresigned)
	s.metrics.LimitRejected()
	s.metrics.In("part", 1)
	if parts(1) != 1 || parts(PartSize) != 1 || parts(PartSize+1) != 2 {
		t.Errorf("the part count of one part, one full part and one byte more is %d, %d, %d",
			parts(1), parts(PartSize), parts(PartSize+1))
	}
}
