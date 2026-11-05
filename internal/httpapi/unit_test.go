package httpapi

import (
	"net/http"
	"testing"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

func TestRequestHashCanonicalizesJSON(t *testing.T) {
	a := requestHash("POST", "/v1/x", []byte(`{"b": 1, "a": [1, 2]}`))
	b := requestHash("POST", "/v1/x", []byte(`{"a":[1,2],"b":1}`))
	if a != b {
		t.Fatal("key order or whitespace changed the fingerprint")
	}
	if a == requestHash("POST", "/v1/y", []byte(`{"a":[1,2],"b":1}`)) {
		t.Fatal("path is not part of the fingerprint")
	}
	if a == requestHash("POST", "/v1/x", []byte(`{"a":[1,2],"b":2}`)) {
		t.Fatal("body value is not part of the fingerprint")
	}
	// Large integers must not lose precision through float64.
	if requestHash("POST", "/", []byte(`{"n":9007199254740993}`)) == requestHash("POST", "/", []byte(`{"n":9007199254740992}`)) {
		t.Fatal("large numbers collapsed to the same fingerprint")
	}
	if requestHash("POST", "/", nil) != requestHash("POST", "/", []byte("  ")) {
		t.Fatal("empty bodies should match")
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"abc", "order-123_retry.1", "A:B"} {
		if !validKey(k) {
			t.Errorf("%q rejected", k)
		}
	}
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	for _, k := range []string{"", "tab\tkey", "kunci-é", string(long)} {
		if validKey(k) {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[domain.ErrorKind]int{
		domain.KindValidation: http.StatusBadRequest,
		domain.KindNotFound:   http.StatusNotFound,
		domain.KindConflict:   http.StatusConflict,
		domain.KindRule:       http.StatusUnprocessableEntity,
	}
	for k, want := range cases {
		if got := statusFor(k); got != want {
			t.Errorf("kind %d -> %d, want %d", k, got, want)
		}
	}
}
