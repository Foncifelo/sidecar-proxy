package events

import (
	"reflect"
	"strings"
	"testing"
)

// forbidden are field-name fragments that would mean content left the customer's
// infrastructure. CONTRIBUTING.md rule 1: if this test fails, do not "fix" the test.
var forbidden = []string{"prompt", "completion", "message", "body", "content", "header", "hash", "input_text", "output_text"}

func TestEventCarriesNoContent(t *testing.T) {
	t.Parallel()
	check := func(typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Errorf("field %s.%s (%q) looks like request content; forbidden", typ.Name(), f.Name, f.Tag.Get("json"))
				}
			}
		}
	}
	check(reflect.TypeOf(Event{}))
	check(reflect.TypeOf(Attempt{}))
}

func TestMoneyIsInteger(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(Event{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if strings.HasPrefix(f.Name, "Cost") && f.Type.Kind() != reflect.Int64 {
			t.Errorf("%s must be Micros (int64), got %s", f.Name, f.Type)
		}
	}
}
