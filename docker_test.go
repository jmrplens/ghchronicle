package ghchronicle

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTheImagesOwnTheDirectoryTheStateIsKeptIn. Docker fills a new named
// volume from the directory it is mounted over, owner included, and the
// collector runs as uid 65532. Up to 2.6.0 neither image had the directory the
// example configuration keeps the state in and every compose stack mounts its
// state volume on, so the volume was created owned by root, and every sweep
// warned "state not saved" and "cache file not saved" and started from
// nothing after a restart (measured with the 2.6.0 image). Both images have
// to carry that directory, owned by the user they run as; the images' own
// behavior was measured by running them, and this holds the lines that give
// it, and holds the example and the stacks to the same directory.
func TestTheImagesOwnTheDirectoryTheStateIsKeptIn(t *testing.T) {
	t.Parallel()
	dir := exampleStateDir(t)

	for _, file := range []string{"Dockerfile", filepath.Join("docker", "Dockerfile.goreleaser")} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			final := finalStage(t, file)
			user := ""
			for _, line := range final {
				if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "USER" {
					user = fields[1]
				}
			}
			if user == "" {
				t.Fatalf("%s names no USER in the stage it ships", file)
			}
			made := slices.ContainsFunc(final, func(line string) bool {
				fields := strings.Fields(line)
				return len(fields) > 2 && fields[0] == "COPY" &&
					slices.Contains(fields, "--chown="+user) &&
					strings.TrimSuffix(fields[len(fields)-1], "/") == dir
			})
			if !made {
				t.Errorf("%s ships no %s owned by %s, the user it runs as, so a new "+
					"named volume mounted there belongs to root", file, dir, user)
			}
		})
	}

	stacks, err := filepath.Glob(filepath.Join("deploy", "compose.*.yaml"))
	if err != nil || len(stacks) == 0 {
		t.Fatalf("no compose stacks under deploy/: %v", err)
	}
	for _, stack := range stacks {
		body, readErr := os.ReadFile(stack)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(body), "- state:"+dir+"\n") {
			t.Errorf("%s does not mount its state volume on %s, the directory the images own", stack, dir)
		}
		if !strings.Contains(string(body), "state_file: "+dir+"/") {
			t.Errorf("%s does not keep its state in %s, the directory its volume is mounted on", stack, dir)
		}
	}
}

// exampleStateDir is the directory the example configuration keeps the state
// file in, which is where the documented containers and every stack keep it.
func exampleStateDir(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		StateFile string `yaml:"state_file"`
	}
	if unmarshalErr := yaml.Unmarshal(body, &example); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if !path.IsAbs(example.StateFile) {
		t.Fatalf("the example's state_file is %q, not an absolute path in a container", example.StateFile)
	}
	return path.Dir(example.StateFile)
}

// finalStage is the instructions after a Dockerfile's last FROM, which are the
// image that ships, with continuation lines joined and comments dropped.
func finalStage(t *testing.T, file string) []string {
	t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var pending strings.Builder
	for raw := range strings.SplitSeq(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if cont, ok := strings.CutSuffix(line, "\\"); ok {
			pending.WriteString(cont + " ")
			continue
		}
		pending.WriteString(line)
		lines = append(lines, pending.String())
		pending.Reset()
	}
	last := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("%s has no FROM", file)
	}
	return lines[last+1:]
}
