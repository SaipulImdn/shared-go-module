package transactionlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBodyLimitFromEnv(t *testing.T) {
	cases := map[string]int{"": 65536, "abc": 65536, "0": 65536, "-5": 65536, "100": 1024, "8192": 8192, "99999999": 98304}
	for in, want := range cases {
		t.Setenv("LOG_BODY_LIMIT_BYTES", in)
		if got := bodyLimitFromEnv(); got != want {
			t.Errorf("LOG_BODY_LIMIT_BYTES=%q -> %d, want %d", in, got, want)
		}
	}
}

func event(status int, rc string, req, resp string) Event {
	now := time.Now()
	return NewEvent("svc", "POST", "/api/v1/x", "rid", "sid", "u", "uid", status, rc, now, now, RequestHeader{}, []byte(req), []byte(resp), int64(len(req)), int64(len(resp)), false)
}

func TestEventCarriesResponseCodeDescAndFullBody(t *testing.T) {
	resp := `{"response_code":"IT","response_desc":"Invalid or expired token.","response_data":{"items":[1,2,3]}}`
	e := event(200, "IT", `{"name":"a"}`, resp)
	if e.RC != "IT" || e.ResponseCode != "IT" || e.ResponseDesc != "Invalid or expired token." {
		t.Fatalf("rc=%q code=%q desc=%q", e.RC, e.ResponseCode, e.ResponseDesc)
	}
	if !strings.Contains(e.ResponseBody, `"items":[1,2,3]`) {
		t.Fatalf("full response body expected, got %s", e.ResponseBody)
	}
}

func TestResponseDescFallsBackToMessageAndIgnoresNonEnvelopes(t *testing.T) {
	if got := responseDesc(`{"message":"boom"}`); got != "boom" {
		t.Errorf("message fallback = %q", got)
	}
	for _, in := range []string{``, `[1,2]`, `not json`, `{"response_desc":5}`} {
		if got := responseDesc(in); got != "" {
			t.Errorf("responseDesc(%q) = %q, want empty", in, got)
		}
	}
}

func TestCredentialsStayMaskedInLoggedBodies(t *testing.T) {
	e := event(200, "00", `{"email":"a@b.c","password":"hunter2","nested":{"refresh_token":"abc","pin":"1234"}}`, `{"response_code":"00","response_data":{"access_token":"tok123","name":"Ann"}}`)
	blob := toText(e.RequestBody) + e.ResponseBody
	for _, secret := range []string{"hunter2", "abc\"", "1234", "tok123"} {
		if strings.Contains(blob, secret) {
			t.Errorf("secret %q leaked: %s", secret, blob)
		}
	}
	if !strings.Contains(blob, "a@b.c") || !strings.Contains(blob, "Ann") {
		t.Errorf("non-credential fields must stay readable: %s", blob)
	}
}

func toText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestLargeBodiesBelowTheLimitAreLoggedInFull(t *testing.T) {
	big := `{"response_code":"00","response_data":"` + strings.Repeat("x", 20000) + `"}` // 20 KB, was omitted at 4 KiB
	e := event(200, "00", "", big)
	if e.ResponseBody != big {
		t.Fatalf("a 20 KB JSON body must be logged in full (len=%d)", len(e.ResponseBody))
	}
}

func TestOversizedBodyBecomesMaskedPreviewWithSizeAndLimit(t *testing.T) {
	t.Setenv("LOG_BODY_LIMIT_BYTES", "1024")
	old := DefaultBodyLimit
	DefaultBodyLimit = bodyLimitFromEnv()
	t.Cleanup(func() { DefaultBodyLimit = old })

	body := `{"access_token":"supersecret","data":"` + strings.Repeat("y", 5000) + `"}`
	e := event(200, "00", "", body)
	for _, want := range []string{`"omitted":"body exceeds log limit"`, `"size":`, `"limit":1024`, `"preview":`} {
		if !strings.Contains(e.ResponseBody, want) {
			t.Errorf("missing %s in %s", want, e.ResponseBody)
		}
	}
	if strings.Contains(e.ResponseBody, "supersecret") {
		t.Errorf("credential leaked in preview: %s", e.ResponseBody)
	}
}

func TestMaskRawHandlesJSONAndURLCredentials(t *testing.T) {
	got := maskRaw(`{"password": "p w", "ok":"fine", "x":"https://h/f?X-Amz-Signature=abc123&a=1&token=zzz"}`)
	for _, leak := range []string{"p w", "abc123", "zzz"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"ok":"fine"`) {
		t.Errorf("benign field lost: %s", got)
	}
}

func TestOmittedBodyTypeNamesTheContentType(t *testing.T) {
	got := string(OmittedBodyType("non-JSON response body", 33070, "image/jpeg"))
	if !strings.Contains(got, `"content_type":"image/jpeg"`) || !strings.Contains(got, `"size":33070`) {
		t.Fatalf("unexpected placeholder: %s", got)
	}
}
