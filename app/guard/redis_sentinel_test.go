package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/redisopt"
)

// With redis.sentinel set, every block that opens a Redis client reaches the
// loader with the Sentinels, the master and the user, the password only as
// ${…} from a Secret, and every container holding the Redis password also
// holds the Sentinel's; without it nothing about Sentinel is rendered (#2505).
func TestRedisSentinelReachesEveryRedisClient(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	text := renderedConfig(t, string(out))
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "sentinel_password" && strings.TrimSpace(v) != `"${YARILO_REDIS_SENTINEL_PASSWORD}"` {
			t.Errorf("the ConfigMap carries a Sentinel password that is not a Secret reference: %s", line)
		}
	}
	for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
		t.Setenv(m[1], "guard-"+m[1])
	}
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	blocks := map[string]redisopt.Sentinel{
		"locks_service":  cfg.LocksService.Sentinel(),
		"auth.token":     cfg.Auth.Token.Sentinel(),
		"warden_service": cfg.WardenService.Sentinel(),
	}
	for name, dc := range cfg.Dicts {
		if dc.Driver == "redis" {
			blocks["dicts."+name] = redisopt.SentinelFromSettings(dc.Settings)
		}
	}
	if len(blocks) < 4 {
		t.Fatalf("only %d Redis blocks rendered, the sandbox has a redis dict too", len(blocks))
	}
	for name, s := range blocks {
		if len(s.Addrs) == 0 || s.Master != "mymaster" || s.User != "sentinel" || s.Password != "guard-YARILO_REDIS_SENTINEL_PASSWORD" {
			t.Errorf("%s reached the loader as %+v, want the Sentinels, mymaster, sentinel and the password from the Secret", name, s)
		}
	}

	for _, d := range renderDocs(t, "-f", "../../helm_values/values-sandbox.yaml") {
		for _, c := range d.Spec.Template.Spec.Containers {
			var redis, sentinel bool
			for _, e := range c.Env {
				redis = redis || e.Name == "YARILO_REDIS_PASSWORD"
				sentinel = sentinel || e.Name == "YARILO_REDIS_SENTINEL_PASSWORD"
			}
			if redis != sentinel {
				t.Errorf("%s/%s has the Redis password %v but the Sentinel password %v", d.Metadata.Name, c.Name, redis, sentinel)
			}
		}
	}

	plain, err := exec.CommandContext(t.Context(), "helm", "template", "yarilo", "../../helm").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	if strings.Contains(strings.ToLower(string(plain)), "sentinel") {
		t.Error("the chart's defaults render something about Sentinel")
	}
}
