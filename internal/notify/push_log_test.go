package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestPushLogFieldsNeverExposeSubscriptionURL(t *testing.T) {
	endpoint := "https://fcm.googleapis.com/fcm/send/sensitive-subscription-token?auth=secret"
	label := pushEndpointLogLabel(endpoint)
	errText := pushErrorLogText(errors.New(`Post "` + endpoint + `": context deadline exceeded`))

	for name, value := range map[string]string{"endpoint label": label, "error text": errText} {
		for _, secret := range []string{"sensitive-subscription-token", "auth=secret", endpoint} {
			if strings.Contains(value, secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, value)
			}
		}
	}
	if !strings.Contains(label, "fcm.googleapis.com#") {
		t.Fatalf("endpoint label should retain a safe host and fingerprint: %s", label)
	}
	if !strings.Contains(errText, "<redacted-url>") {
		t.Fatalf("error text should retain a redaction marker: %s", errText)
	}
}
