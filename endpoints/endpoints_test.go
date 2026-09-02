package endpoints

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a:1", []string{"a:1"}},
		{"a:1,b:2", []string{"a:1", "b:2"}},
		{" a:1 , b:2 ", []string{"a:1", "b:2"}},
		{"a:1,,b:2,", []string{"a:1", "b:2"}},
		{",", nil},
	}
	for _, c := range cases {
		got := Split(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestSelect_EmptyList(t *testing.T) {
	ep, err := Select(nil, func(string) error { return nil })
	if ep != "" || !errors.Is(err, ErrNoEndpoints) {
		t.Fatalf("got (%q, %v), want an empty endpoint and ErrNoEndpoints", ep, err)
	}
}

// The zero-regression rule: one endpoint is never probed.
func TestSelect_SingleEndpointIsNotProbed(t *testing.T) {
	probed := false
	ep, err := Select([]string{"only:1"}, func(string) error {
		probed = true
		return errors.New("must not be called")
	})
	if err != nil || ep != "only:1" {
		t.Fatalf("got (%q, %v), want (only:1, nil)", ep, err)
	}
	if probed {
		t.Error("a single-endpoint list must not be probed")
	}
}

func TestSelect_FirstHealthyWins(t *testing.T) {
	var seen []string
	ep, err := Select([]string{"a", "b", "c"}, func(e string) error {
		seen = append(seen, e)
		return nil
	})
	if err != nil || ep != "a" {
		t.Fatalf("got (%q, %v), want (a, nil)", ep, err)
	}
	if !reflect.DeepEqual(seen, []string{"a"}) {
		t.Errorf("probed %v, want to stop at the first success", seen)
	}
}

func TestSelect_FallsBackInOrder(t *testing.T) {
	var seen []string
	ep, err := Select([]string{"pr", "dev", "last"}, func(e string) error {
		seen = append(seen, e)
		if e == "pr" {
			return errors.New("no such host")
		}
		return nil
	})
	if err != nil || ep != "dev" {
		t.Fatalf("got (%q, %v), want (dev, nil)", ep, err)
	}
	if !reflect.DeepEqual(seen, []string{"pr", "dev"}) {
		t.Errorf("probed %v, want [pr dev]", seen)
	}
}

// All down: a usable endpoint plus an error that names every failure. Never
// empty, never nil-with-no-value.
func TestSelect_AllFailReturnsFirstAndError(t *testing.T) {
	boom := errors.New("boom")
	ep, err := Select([]string{"a", "b"}, func(string) error { return boom })
	if ep != "a" {
		t.Errorf("got %q, want the first endpoint back", ep)
	}
	if err == nil {
		t.Fatal("want an error when every probe fails")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the probe failures", err)
	}
}

func TestSelect_NilProbeTakesTheFirst(t *testing.T) {
	ep, err := Select([]string{"a", "b"}, nil)
	if err != nil || ep != "a" {
		t.Fatalf("got (%q, %v), want (a, nil)", ep, err)
	}
}
