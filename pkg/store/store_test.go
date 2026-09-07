package store

import (
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

func open(t *testing.T) (*Store, *Collection[api.Message, *api.Message]) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, For[api.Message, *api.Message](s)
}

func msg(name, conv string) *api.Message {
	return &api.Message{
		TypeMeta: api.TypeMeta{APIVersion: api.Version, Kind: api.KindMessage},
		Metadata: api.ObjectMeta{Name: name},
		Spec:     api.MessageSpec{Conversation: conv, Body: api.Body{Text: "hi"}},
	}
}

func TestCreateStampsIdentity(t *testing.T) {
	_, c := open(t)
	got, err := c.Create(msg("m1", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case got.Metadata.UID == "":
		t.Error("no uid assigned")
	case got.Metadata.ResourceVersion == 0:
		t.Error("no resource version assigned")
	case got.Metadata.Generation != 1:
		t.Errorf("generation = %d, want 1", got.Metadata.Generation)
	case got.Metadata.CreationTimestamp.IsZero():
		t.Error("no creation timestamp")
	}
}

func TestCreateRefusesDuplicateName(t *testing.T) {
	_, c := open(t)
	if _, err := c.Create(msg("m1", "conv")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Create(msg("m1", "conv"))
	if !api.IsAlreadyExists(err) {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
}

func TestGenerateName(t *testing.T) {
	_, c := open(t)
	seen := map[string]bool{}
	for range 20 {
		m := msg("", "conv")
		m.Metadata.GenerateName = "conv-"
		got, err := c.Create(m)
		if err != nil {
			t.Fatal(err)
		}
		if seen[got.Metadata.Name] {
			t.Fatalf("generated a name twice: %s", got.Metadata.Name)
		}
		seen[got.Metadata.Name] = true
		if got.Metadata.GenerateName != "" {
			t.Error("generateName should be cleared once a name is assigned")
		}
	}
}

// Names arrive from MCP callers and become file paths, so this is a boundary,
// not a formality.
func TestCreateRejectsUnsafeNames(t *testing.T) {
	_, c := open(t)
	for _, name := range []string{"", "../escape", "a/b", `a\b`, ".hidden", "with\x00null"} {
		if _, err := c.Create(msg(name, "conv")); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}

// Optimistic concurrency: a write made against a stale read is refused, so two
// writers cannot silently overwrite each other.
func TestUpdateRefusesStaleVersion(t *testing.T) {
	_, c := open(t)
	first, err := c.Create(msg("m1", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	stale := *first

	first.Spec.Body.Text = "changed"
	if _, err := c.Update(first); err != nil {
		t.Fatal(err)
	}

	stale.Spec.Body.Text = "also changed"
	_, err = c.Update(&stale)
	if !api.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
	// Version zero is "I have not read it, overwrite" — available on purpose for
	// the daemon's own reconcilers.
	stale.Metadata.ResourceVersion = 0
	if _, err := c.Update(&stale); err != nil {
		t.Errorf("an unconditional update should be allowed: %v", err)
	}
}

// The generation is what lets a controller tell a change of intent from its own
// status write coming back around.
func TestGenerationTracksSpecNotStatus(t *testing.T) {
	_, c := open(t)
	m, err := c.Create(msg("m1", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	m.Status.Phase = api.PhaseSent
	afterStatus, err := c.UpdateStatus(m)
	if err != nil {
		t.Fatal(err)
	}
	if afterStatus.Metadata.Generation != 1 {
		t.Errorf("a status write bumped the generation to %d", afterStatus.Metadata.Generation)
	}
	if afterStatus.Metadata.ResourceVersion == m.Metadata.ResourceVersion {
		t.Error("a status write must still advance the resource version")
	}
	afterStatus.Spec.Body.Text = "new intent"
	afterSpec, err := c.Update(afterStatus)
	if err != nil {
		t.Fatal(err)
	}
	if afterSpec.Metadata.Generation != 2 {
		t.Errorf("generation = %d after a spec change, want 2", afterSpec.Metadata.Generation)
	}
}

// A client must not be able to re-date an object or adopt another one's uid.
func TestUpdateKeepsIdentity(t *testing.T) {
	_, c := open(t)
	m, err := c.Create(msg("m1", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	uid, born := m.Metadata.UID, m.Metadata.CreationTimestamp
	m.Metadata.UID = "forged"
	m.Metadata.CreationTimestamp = time.Unix(0, 0).UTC()
	got, err := c.Update(m)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.UID != uid || !got.Metadata.CreationTimestamp.Equal(born) {
		t.Errorf("identity was overwritten by the client: %+v", got.Metadata)
	}
}

func TestDeleteAndNotFound(t *testing.T) {
	_, c := open(t)
	if _, err := c.Create(msg("m1", "conv")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("m1"); !api.IsNotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
	if err := c.Delete("m1"); !api.IsNotFound(err) {
		t.Errorf("deleting twice: err = %v, want NotFound", err)
	}
}

// The store is a directory of files: everything must survive a restart, the
// version counter included, so a resumed watch is not handed versions that were
// already used.
func TestReopenKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := For[api.Message, *api.Message](s)
	if _, err := c.Create(msg("m1", "conv")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(msg("m2", "conv")); err != nil {
		t.Fatal(err)
	}
	version := s.Version()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version() < version {
		t.Errorf("version went backwards across a restart: %d then %d", version, s2.Version())
	}
	c2 := For[api.Message, *api.Message](s2)
	if _, err := c2.Get("m2"); err != nil {
		t.Errorf("m2 did not survive: %v", err)
	}
	if _, err := c2.Get("m1"); !api.IsNotFound(err) {
		t.Error("a deleted object came back")
	}
}

func TestListIsSortedAndCarriesTheVersion(t *testing.T) {
	s, c := open(t)
	for _, n := range []string{"c", "a", "b"} {
		if _, err := c.Create(msg(n, "conv")); err != nil {
			t.Fatal(err)
		}
	}
	items, rv, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Metadata.Name != "a" || items[2].Metadata.Name != "c" {
		t.Errorf("listing is not sorted: %+v", items)
	}
	if rv != s.Version() {
		t.Errorf("list version %d != store version %d", rv, s.Version())
	}
}

func TestWatchDeliversChanges(t *testing.T) {
	s, c := open(t)
	events, cancel, err := s.Watch(0, api.KindMessage)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if _, err := c.Create(msg("m1", "conv")); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Type != api.Added || ev.Name != "m1" {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// Listing then watching from the list's version is the contract that makes an
// agent's collection exactly-once: nothing between the two is lost.
func TestWatchReplaysFromAVersion(t *testing.T) {
	s, c := open(t)
	if _, err := c.Create(msg("before", "conv")); err != nil {
		t.Fatal(err)
	}
	_, rv, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(msg("after", "conv")); err != nil {
		t.Fatal(err)
	}

	events, cancel, err := s.Watch(rv, api.KindMessage)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case ev := <-events:
		if ev.Name != "after" {
			t.Errorf("replayed %q; a watch from a list's version must skip what the list already had", ev.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the change made after the list was not replayed")
	}
}

func TestWatchFiltersByKind(t *testing.T) {
	s, c := open(t)
	events, cancel, err := s.Watch(0, api.KindChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := c.Create(msg("m1", "conv")); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		t.Errorf("a Channel watch received %s/%s", ev.Kind, ev.Name)
	case <-time.After(300 * time.Millisecond):
	}
}

// A version the store no longer retains is refused, because the alternative is
// a stream that quietly skipped what the client missed.
func TestWatchRefusesAVersionItCannotHonour(t *testing.T) {
	s, c := open(t)
	for i := range historyKept + 10 {
		if _, err := c.Create(msg("m"+itoa(i), "conv")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Watch(1, api.KindMessage); !api.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
