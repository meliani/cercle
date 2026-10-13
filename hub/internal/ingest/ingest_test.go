package ingest

import "testing"

func TestDecode(t *testing.T) {
	evs, err := Decode([]byte(`{"type":"scrobble","user":{"username":"alice"},"extra":1}`))
	if err != nil || len(evs) != 1 || evs[0].User.Username != "alice" {
		t.Fatalf("single: %+v %v", evs, err)
	}
	evs, err = Decode([]byte(`{"events":[{"type":"a"},{"type":"b"}]}`))
	if err != nil || len(evs) != 2 {
		t.Fatalf("batch: %+v %v", evs, err)
	}
	for _, bad := range []string{``, `[]`, `{"events":"x"}`, `{nope`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{
		"starting": "playing", "playing": "playing", "paused": "paused",
		"stopped": "stopped", "expired": "stopped", "PLAYING": "playing", "weird": "",
	} {
		if got := NormalizeState(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}
