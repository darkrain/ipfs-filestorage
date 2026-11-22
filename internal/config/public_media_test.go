package config

import (
	"testing"
	"time"
)

func TestPublicMediaFromEnvironment(t *testing.T) {
	t.Setenv("PUBLIC_MEDIA_PACKAGES", " first = bafyA , model=bafyB,broken")
	t.Setenv("PUBLIC_MEDIA_ORIGINS", "https://iamfree24.com, https://iamtopmember.com")
	t.Setenv("PUBLIC_MEDIA_LINK_TTL", "30m")
	t.Setenv("PUBLIC_MEDIA_ALLOW_NO_ORIGIN", "false")
	cfg := Load().PublicMedia
	if cfg.Packages["first"] != "bafyA" || cfg.Packages["model"] != "bafyB" || len(cfg.Packages) != 3 {
		t.Fatalf("packages %v", cfg.Packages)
	}
	if _, kept := cfg.Packages["broken"]; !kept {
		t.Fatal("a malformed pair must reach validation, not vanish")
	}
	if len(cfg.Origins) != 2 || cfg.LinkTTL != 30*time.Minute || cfg.AllowNoOrigin {
		t.Fatalf("%+v", cfg)
	}
}

func TestPublicMediaIsOffByDefault(t *testing.T) {
	cfg := Load().PublicMedia
	if len(cfg.Packages) != 0 || cfg.LinkTTL != 45*time.Minute || !cfg.AllowNoOrigin || cfg.RateLimit.RPS != 20 {
		t.Fatalf("%+v", cfg)
	}
}
