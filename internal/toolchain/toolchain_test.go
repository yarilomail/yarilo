package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// go.mod and the base image must name one toolchain: the scan reads go.mod,
// the image ships the base. They disagreed silently, and both numbers were
// right about different toolchains (#1497).
func TestGoModAndDockerfileNameTheSameToolchain(t *testing.T) {
	root := filepath.Join("..", "..")

	gomod := readFile(t, filepath.Join(root, "go.mod"))
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`).FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod has no `go X.Y.Z` line; an unpinned patch version is what this guards against")
	}
	want := m[1]
	if tc := regexp.MustCompile(`(?m)^toolchain go(\S+)$`).FindStringSubmatch(gomod); tc != nil && tc[1] != want {
		t.Errorf("go.mod: toolchain go%s, go %s", tc[1], want)
	}

	stages := golangStages(t, readFile(t, filepath.Join(root, "docker", "Dockerfile")))
	if len(stages) == 0 {
		t.Fatal("no golang base image found in docker/Dockerfile; this guard is watching a file that moved")
	}
	for _, st := range stages {
		if version, _, _ := strings.Cut(st.tag, "-alpine"); version != want {
			t.Errorf("stage %q builds on golang:%s, go.mod says %s", st.name, st.tag, want)
		}
		// Per stage, because ENV does not cross a FROM: without this the fts
		// stage -- which builds a binary we ship -- could lose the line while
		// the builder stage kept it, and the check would still pass.
		if !strings.Contains(st.body, "ENV GOTOOLCHAIN=local") {
			t.Errorf("stage %q does not set GOTOOLCHAIN=local, so a newer go.mod fetches a toolchain instead of failing the build", st.name)
		}
	}
}

type dockerStage struct {
	name string
	tag  string
	body string
}

// golangStages splits the Dockerfile at each FROM and returns the golang ones,
// each with the text that belongs to it alone.
func golangStages(t *testing.T, dockerfile string) []dockerStage {
	t.Helper()
	from := regexp.MustCompile(`(?m)^FROM (\S+)(?: AS (\S+))?`)
	locs := from.FindAllStringSubmatchIndex(dockerfile, -1)
	var out []dockerStage
	for i, loc := range locs {
		end := len(dockerfile)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		image := dockerfile[loc[2]:loc[3]]
		if !strings.HasPrefix(image, "golang:") {
			continue
		}
		name := image
		if loc[4] >= 0 {
			name = dockerfile[loc[4]:loc[5]]
		}
		tag, _, _ := strings.Cut(strings.TrimPrefix(image, "golang:"), "@")
		out = append(out, dockerStage{
			name: name,
			tag:  tag,
			body: dockerfile[loc[0]:end],
		})
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// Every setup-go step takes its release from go.mod: a go-version of its own
// lets the gate test another compiler than the image ships.
func TestEverySetupGoReadsGoMod(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	steps := 0
	for _, f := range files {
		lines := strings.Split(readFile(t, f), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "uses: actions/setup-go@") {
				continue
			}
			steps++
			indent := len(line) - len(strings.TrimLeft(line, " -"))
			var fromMod, own bool
			for _, l := range lines[i+1:] {
				body := strings.TrimLeft(l, " ")
				if body == "" || strings.HasPrefix(body, "#") {
					continue
				}
				if len(l)-len(body) < indent {
					break
				}
				fromMod = fromMod || strings.TrimSpace(body) == "go-version-file: go.mod"
				own = own || strings.HasPrefix(body, "go-version:")
			}
			if !fromMod || own {
				t.Errorf("%s:%d: setup-go must take go-version-file: go.mod and no go-version", filepath.Base(f), i+1)
			}
		}
	}
	if steps == 0 {
		t.Fatal("no setup-go step found; this guard is watching files that moved")
	}
}

// The README badge names the minor release go.mod builds with.
func TestTheReadmeBadgeIsTheGoModMinor(t *testing.T) {
	root := filepath.Join("..", "..")
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+)\.\d+$`).FindStringSubmatch(readFile(t, filepath.Join(root, "go.mod")))
	if m == nil {
		t.Fatal("go.mod has no `go X.Y.Z` line")
	}
	badge := regexp.MustCompile(`badge/Go-(\d+\.\d+)-`).FindStringSubmatch(readFile(t, filepath.Join(root, "README.md")))
	if badge == nil {
		t.Fatal("README.md has no Go badge")
	}
	if badge[1] != m[1] {
		t.Errorf("README.md: Go badge %s, go.mod %s", badge[1], m[1])
	}
}

// Every base image is pinned by digest: a tag can be moved under the build.
func TestEveryBaseImageIsPinnedByDigest(t *testing.T) {
	dockerfile := readFile(t, filepath.Join("..", "..", "docker", "Dockerfile"))
	from := regexp.MustCompile(`(?m)^FROM (\S+)`)
	for _, m := range from.FindAllStringSubmatch(dockerfile, -1) {
		if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(m[1]) {
			t.Errorf("FROM %s is not pinned by digest", m[1])
		}
	}
}
