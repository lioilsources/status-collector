package checker

import (
	"strings"
	"testing"
)

func TestServiceTokenOnlyOnCloudflareEndpoints(t *testing.T) {
	t.Setenv("CF_ACCESS_CLIENT_ID", "abc.access")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "s3cret")

	eps := DefaultEndpoints("http://192.168.1.50:8188")
	if len(eps) == 0 {
		t.Fatal("no endpoints")
	}

	for _, ep := range eps {
		viaCloudflare := strings.HasPrefix(ep.URL, "https://") && strings.Contains(ep.URL, ".ol1n.com")
		got := ep.Headers["CF-Access-Client-Id"]
		if viaCloudflare && got != "abc.access" {
			t.Errorf("%s goes through Cloudflare Access but carries no token", ep.ID)
		}
		// Sending the token to a LAN box would leak it outside its audience.
		if !viaCloudflare && got != "" {
			t.Errorf("%s (%s) is direct but carries the service token", ep.ID, ep.URL)
		}
	}
}

func TestNoServiceTokenWhenEnvUnset(t *testing.T) {
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

	for _, ep := range DefaultEndpoints("") {
		if len(ep.Headers) != 0 {
			t.Errorf("%s got headers %v with no token configured", ep.ID, ep.Headers)
		}
	}
}

func TestPartialTokenIsNotSent(t *testing.T) {
	// A half-configured token would be sent as a malformed header and rejected;
	// better to behave exactly as if nothing was configured.
	t.Setenv("CF_ACCESS_CLIENT_ID", "abc.access")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

	if h := cfAccessHeaders(); h != nil {
		t.Errorf("expected no headers from a partial token, got %v", h)
	}
}

func TestComfyEndpointsFollowTheFlag(t *testing.T) {
	none := DefaultEndpoints("")
	for _, ep := range none {
		if ep.Group == "ComfyUI" {
			t.Errorf("ComfyUI endpoint %s present without -comfy", ep.ID)
		}
	}

	with := DefaultEndpoints("http://192.168.1.50:8188/")
	var urls []string
	for _, ep := range with {
		if ep.Group == "ComfyUI" {
			urls = append(urls, ep.URL)
		}
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 ComfyUI endpoints, got %d", len(urls))
	}
	// The trailing slash on the flag must not produce a double slash.
	for _, u := range urls {
		if strings.Contains(strings.TrimPrefix(u, "http://"), "//") {
			t.Errorf("malformed ComfyUI URL: %s", u)
		}
	}
}
