/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package datapol

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	marker = "hunter2"
)

type withDatapolTag struct {
	Key string `json:"key" datapolicy:"password"`
}

type withExternalType struct {
	Header http.Header `json:"header"`
}

type noDatapol struct {
	Key string `json:"key"`
}

type datapolInMember struct {
	secrets withDatapolTag
}

type datapolInSlice struct {
	secrets []withDatapolTag
}

type datapolInMap struct {
	secrets map[string]withDatapolTag
}

type datapolBehindPointer struct {
	secrets *withDatapolTag
}

func TestValidate(t *testing.T) {
	testcases := []struct {
		name      string
		value     interface{}
		expect    []string
		badFilter bool
	}{{
		name:   "Empty password",
		value:  withDatapolTag{},
		expect: []string{},
	}, {
		name: "Non-empty password",
		value: withDatapolTag{
			Key: marker,
		},
		expect: []string{"password"},
	}, {
		name:   "empty external type",
		value:  withExternalType{Header: http.Header{}},
		expect: []string{},
	}, {
		name: "external type",
		value: withExternalType{Header: http.Header{
			"Authorization": []string{"Bearer hunter2"},
		}},
		expect: []string{"password", "token"},
	}, {
		name:      "no datapol tag",
		value:     noDatapol{Key: marker},
		expect:    []string{},
		badFilter: true,
	}, {
		name: "nested",
		value: datapolInMember{
			secrets: withDatapolTag{
				Key: marker,
			},
		},
		expect: []string{"password"},
	}, {
		name: "nested in pointer",
		value: datapolBehindPointer{
			secrets: &withDatapolTag{Key: marker},
		},
		expect: []string{},
	}, {
		name: "nested in slice",
		value: datapolInSlice{
			secrets: []withDatapolTag{{Key: marker}},
		},
		expect: []string{"password"},
	}, {
		name: "nested in map",
		value: datapolInMap{
			secrets: map[string]withDatapolTag{
				"key": {Key: marker},
			},
		},
		expect: []string{"password"},
	}, {
		name: "nested in map but empty",
		value: datapolInMap{
			secrets: map[string]withDatapolTag{
				"key": {},
			},
		},
		expect: []string{},
	}, {
		name: "struct in interface",
		value: struct{ v interface{} }{v: withDatapolTag{
			Key: marker,
		}},
		expect: []string{"password"},
	}, {
		name: "structptr in interface",
		value: struct{ v interface{} }{v: &withDatapolTag{
			Key: marker,
		}},
		expect: []string{},
	}}
	for _, tc := range testcases {
		res := Verify(tc.value)
		if !assert.ElementsMatch(t, tc.expect, res) {
			t.Errorf("Wrong set of tags for %q. expect %v, got %v", tc.name, tc.expect, res)
		}
		if !tc.badFilter {
			formatted := fmt.Sprintf("%v", tc.value)
			if strings.Contains(formatted, marker) != (len(tc.expect) > 0) {
				t.Errorf("Filter decision doesn't match formatted value for %q: tags: %v, format: %s", tc.name, tc.expect, formatted)
			}
		}
	}
}

// The following types use exported fields because Redact mutates via
// reflection, which can only set exported fields.

type redactString struct {
	Token  string `datapolicy:"token"`
	Public string
}

// redactHeader mirrors the StaticPodURLHeader shape (map[string][]string) that
// leaked credentials in kubernetes/kubernetes#140101.
type redactHeader struct {
	StaticPodURLHeader map[string][]string `datapolicy:"token"`
	PublicMap          map[string][]string
}

type redactBytes struct {
	Secret []byte `datapolicy:"secret-key"`
}

type redactStringSlice struct {
	Secrets []string `datapolicy:"password"`
}

type redactNested struct {
	Inner  redactString
	Public string
}

type redactPointer struct {
	Inner *redactString
}

type redactNoTags struct {
	A string
	B int
	C map[string]string
}

func TestRedact(t *testing.T) {
	testcases := []struct {
		name   string
		value  interface{}
		expect interface{}
	}{{
		name:   "string field with datapolicy tag is redacted, untagged field preserved",
		value:  &redactString{Token: marker, Public: "visible"},
		expect: &redactString{Token: redacted, Public: "visible"},
	}, {
		name:   "empty tagged string is still redacted",
		value:  &redactString{Token: "", Public: "visible"},
		expect: &redactString{Token: redacted, Public: "visible"},
	}, {
		name: "map[string][]string tagged field preserves keys, redacts values",
		value: &redactHeader{
			StaticPodURLHeader: map[string][]string{
				"Authorization": {"Bearer hunter2"},
				"X-Custom":      {"a", "b"},
			},
			PublicMap: map[string][]string{
				"Accept": {"application/json"},
			},
		},
		expect: &redactHeader{
			StaticPodURLHeader: map[string][]string{
				"Authorization": {redacted},
				"X-Custom":      {redacted},
			},
			PublicMap: map[string][]string{
				"Accept": {"application/json"},
			},
		},
	}, {
		name:   "byte slice tagged field is redacted",
		value:  &redactBytes{Secret: []byte(marker)},
		expect: &redactBytes{Secret: []byte(redacted)},
	}, {
		name:   "string slice tagged field is redacted to single sentinel",
		value:  &redactStringSlice{Secrets: []string{marker, "another"}},
		expect: &redactStringSlice{Secrets: []string{redacted}},
	}, {
		name:   "nested struct with tagged field is redacted, siblings preserved",
		value:  &redactNested{Inner: redactString{Token: marker, Public: "keep"}, Public: "top"},
		expect: &redactNested{Inner: redactString{Token: redacted, Public: "keep"}, Public: "top"},
	}, {
		name:   "tagged field behind pointer is redacted",
		value:  &redactPointer{Inner: &redactString{Token: marker, Public: "keep"}},
		expect: &redactPointer{Inner: &redactString{Token: redacted, Public: "keep"}},
	}, {
		name:   "nil pointer field is left untouched",
		value:  &redactPointer{Inner: nil},
		expect: &redactPointer{Inner: nil},
	}, {
		name:   "struct with no datapolicy tags is a no-op",
		value:  &redactNoTags{A: "a", B: 1, C: map[string]string{"k": "v"}},
		expect: &redactNoTags{A: "a", B: 1, C: map[string]string{"k": "v"}},
	}, {
		name:   "tagged field inside a by-value interface is redacted",
		value:  &struct{ V interface{} }{V: redactString{Token: marker, Public: "keep"}},
		expect: &struct{ V interface{} }{V: redactString{Token: redacted, Public: "keep"}},
	}, {
		name:   "tagged field inside a pointer-in-interface is redacted",
		value:  &struct{ V interface{} }{V: &redactString{Token: marker, Public: "keep"}},
		expect: &struct{ V interface{} }{V: &redactString{Token: redacted, Public: "keep"}},
	}}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			Redact(tc.value)
			assert.Equal(t, tc.expect, tc.value)
		})
	}
}

// TestRedactNilAndNonPointer verifies Redact does not panic on inputs it cannot
// mutate.
func TestRedactNilAndNonPointer(t *testing.T) {
	// nil interface
	Redact(nil)
	// non-pointer struct cannot be mutated but must not panic
	v := redactString{Token: marker}
	Redact(v)
}

// redactCyclic is a self-referential type used to prove Redact does not recurse
// forever on a cyclic object graph. A stack overflow would be a fatal error that
// Redact's recover() cannot catch.
type redactCyclic struct {
	Token string `datapolicy:"token"`
	Self  *redactCyclic
	Peers map[string]*redactCyclic
}

func TestRedactCyclic(t *testing.T) {
	// Pointer cycle: node references itself.
	node := &redactCyclic{Token: marker}
	node.Self = node
	node.Peers = map[string]*redactCyclic{"self": node}

	Redact(node)

	if node.Token != redacted {
		t.Errorf("Token not redacted on cyclic input: got %q", node.Token)
	}
	// The cycle must have been broken (no crash) and the same node reached
	// through the cycle is the already-redacted node.
	if node.Self.Token != redacted {
		t.Errorf("Token not redacted through cycle: got %q", node.Self.Token)
	}
}
