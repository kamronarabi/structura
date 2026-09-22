package dockerfile_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/dockerfile"
)

// Two in five of the Dockerfiles in the test corpus open a heredoc. Their
// bodies are arbitrary text, and a line-by-line reading takes whatever is in
// them for an instruction of this build.
func TestHeredocBodiesAreNotInstructions(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20-alpine",
		"EXPOSE 3000",
		// A script that writes a config file, which is exactly where a line
		// beginning with ENV or FROM turns up in real repositories.
		"RUN <<EOF",
		"cat > /app/.env <<'INNER'",
		"ENV DATABASE_URL=postgres://not-this-services-database:5432/other",
		"INNER",
		"EXPOSE 9999",
		"FROM scratch",
		"EOF",
		"CMD [\"node\", \"index.js\"]",
	}, "\n"))

	n := c.node(t)
	if n.Attrs["baseImage"] != "node:20-alpine" {
		t.Errorf("baseImage = %v: a FROM inside a heredoc was read as a stage", n.Attrs["baseImage"])
	}
	if n.Attrs["buildStages"] != nil {
		t.Errorf("buildStages = %v, want none: the file has one FROM", n.Attrs["buildStages"])
	}
	ports, _ := n.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 3000 {
		t.Errorf("ports = %v, want [3000]: an EXPOSE inside a heredoc is not this image's", ports)
	}
	if len(c.hints) != 0 {
		t.Errorf("hints = %+v, want none: that database belongs to a file this build writes, "+
			"not to this component", c.hints)
	}
}

func TestHeredocForms(t *testing.T) {
	// Each body holds an EXPOSE that must not be counted. Only the real one
	// outside the body may appear.
	tests := map[string]string{
		"bare terminator":   "RUN <<EOF\nEXPOSE 1111\nEOF\n",
		"quoted terminator": "RUN <<\"EOF\"\nEXPOSE 1111\nEOF\n",
		"single quoted":     "RUN <<'EOF'\nEXPOSE 1111\nEOF\n",
		// <<- allows the terminator to be indented.
		"indented terminator": "RUN <<-EOF\n\tEXPOSE 1111\n\tEOF\n",
		"copy into a file":    "COPY <<EOF /app/config\nEXPOSE 1111\nEOF\n",
		// One instruction may open several bodies, closed in order.
		"two on one instruction": "COPY <<A <<B /app/\nEXPOSE 1111\nA\nEXPOSE 2222\nB\n",
		"lowercase instruction":  "run <<EOF\nEXPOSE 1111\nEOF\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			n := extract(t, "Dockerfile", "FROM alpine:3.20\nEXPOSE 8080\n"+body).node(t)
			ports, _ := n.Attrs["ports"].([]int)
			if len(ports) != 1 || ports[0] != 8080 {
				t.Errorf("ports = %v, want [8080]: the heredoc body was read as instructions", ports)
			}
		})
	}
}

// A heredoc nobody closed swallows the rest of the file. That loses
// information, which is the right failure: the alternative is reading a
// shell script as though it described this image.
func TestUnterminatedHeredocEndsTheFile(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"FROM alpine:3.20",
		"EXPOSE 8080",
		"RUN <<EOF",
		"echo still inside",
		"EXPOSE 1111",
	}, "\n")).node(t)

	ports, _ := n.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]", ports)
	}
}

// A redirection that is not a heredoc must not swallow anything.
func TestShellRedirectionIsNotAHeredoc(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"FROM alpine:3.20",
		"RUN echo hi > /tmp/a && cat < /tmp/a",
		"EXPOSE 8080",
	}, "\n")).node(t)

	ports, _ := n.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]: a plain redirection ended the file", ports)
	}
}

// The pattern that prompted this: reporting the two debug ports while dropping
// the one the service listens on is a confident wrong answer, not a gap.
func TestExposeIsReadThroughTheFilesOwnVariables(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:lts AS development",
		"ARG PORT=80",
		"ENV PORT $PORT",
		"EXPOSE $PORT 9229 9230",
	}, "\n")).node(t)

	ports, _ := n.Attrs["ports"].([]int)
	want := []int{80, 9229, 9230}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

func TestVariableForms(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"braced", "FROM node:20\nENV PORT=8080\nEXPOSE ${PORT}\n", 8080},
		{"bare", "FROM node:20\nENV PORT=8080\nEXPOSE $PORT\n", 8080},
		{"arg only", "FROM node:20\nARG PORT=8080\nEXPOSE $PORT\n", 8080},
		{"with a protocol", "FROM node:20\nENV PORT=8080\nEXPOSE $PORT/tcp\n", 8080},
		{"last assignment wins", "FROM node:20\nENV PORT=1\nENV PORT=8080\nEXPOSE $PORT\n", 8080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := extract(t, "Dockerfile", tt.content).node(t)
			ports, _ := n.Attrs["ports"].([]int)
			if len(ports) != 1 || ports[0] != tt.want {
				t.Errorf("ports = %v, want [%d]", ports, tt.want)
			}
		})
	}
}

// Anything the file does not define is decided at run time and names no port.
func TestUnresolvableVariablesYieldNoPort(t *testing.T) {
	for _, content := range []string{
		"FROM node:20\nEXPOSE $PORT\n",
		// Self-referential: one pass, so this cannot loop and cannot resolve.
		"FROM node:20\nARG PORT=$PORT\nEXPOSE $PORT\n",
		// Shell default syntax is deliberately not understood.
		"FROM node:20\nEXPOSE ${PORT:-8080}\n",
		"FROM node:20\nENV PORT=notanumber\nEXPOSE $PORT\n",
		"FROM node:20\nENV PORT=99999\nEXPOSE $PORT\n",
	} {
		n := extract(t, "Dockerfile", content).node(t)
		if ports, _ := n.Attrs["ports"].([]int); len(ports) != 0 {
			t.Errorf("%q gave ports = %v, want none", content, ports)
		}
	}
}

// The asymmetry is deliberate. A port is a fact about the image; a hostname
// default is the value for whoever builds without supplying one, and putting
// it on the diagram would invent a component.
func TestHostnamesAreNotSubstituted(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20",
		"ARG DB_HOST=db.build-default.invalid",
		"ENV DATABASE_URL=postgres://$DB_HOST:5432/app",
	}, "\n"))

	if len(c.hints) != 0 {
		t.Errorf("hints = %+v, want none: a build-time default is not an endpoint", c.hints)
	}
}

func TestEnvValueQuoting(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "several pairs on one line",
			content: "ENV A_URL=https://a.example.com B_URL=https://b.example.com",
			want:    []string{"a.example.com", "b.example.com"},
		},
		{
			name:    "a quoted value containing a space",
			content: "ENV NAME=\"two words\" API_URL=https://c.example.com",
			want:    []string{"c.example.com"},
		},
		{
			name:    "single quotes",
			content: "ENV API_URL='https://d.example.com'",
			want:    []string{"d.example.com"},
		},
		{
			name:    "the legacy space-separated form",
			content: "ENV API_URL https://e.example.com",
			want:    []string{"e.example.com"},
		},
		{
			name:    "a legacy value in quotes",
			content: "ENV API_URL \"https://f.example.com\"",
			want:    []string{"f.example.com"},
		},
		{
			name: "an unterminated quote is not guessed at",
			// Where the value ends is unknowable, and guessing invents a host.
			content: "ENV API_URL=\"https://g.example.com",
			want:    nil,
		},
		{
			name:    "an empty value",
			content: "ENV API_URL=",
			want:    nil,
		},
		{
			name:    "no value at all",
			content: "ENV API_URL",
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := extract(t, "Dockerfile", "FROM node:20\n"+tt.content+"\n")
			var got []string
			for _, h := range c.hints {
				got = append(got, h.Tokens[0])
			}
			if len(got) != len(tt.want) {
				t.Fatalf("tokens = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("tokens = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// ARG has two different standings, and the difference is the whole reason the
// two are not read the same way: it may say what port the image listens on,
// and it may not put a component on the diagram, because a build argument does
// not exist in the running container.
func TestArgSaysWhatPortButNeverWhatEndpoint(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20",
		"ARG PORT=8080",
		"ARG API_URL=https://h.example.com",
		"EXPOSE $PORT",
	}, "\n"))

	n := c.node(t)
	if ports, _ := n.Attrs["ports"].([]int); len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]", n.Attrs["ports"])
	}
	if len(c.hints) != 0 {
		t.Errorf("hints = %+v, want none: an ARG default is a parameter to the build", c.hints)
	}
}

// The promotion idiom is the known cost of that rule, and it is a gap rather
// than a wrong answer. Pinned so that a change here is a decision.
func TestPromotedArgIsNotRecovered(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20",
		"ARG API_URL=https://promoted.example.com",
		"ENV API_URL=$API_URL",
	}, "\n"))

	if len(c.hints) != 0 {
		t.Errorf("hints = %+v: recovering this would mean substituting build defaults "+
			"into hostnames, which are localhost far more often than a real dependency", c.hints)
	}
}

func TestName(t *testing.T) {
	if got := newExtractor().Name(); got != "dockerfile" {
		t.Errorf("Name() = %q, want dockerfile", got)
	}
}

// The extractor runs on untrusted input and must not panic, and must not
// invent the two things a wrong answer would be built from: a component with
// no name, or a reference token that is not a token.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		"FROM node:20\nEXPOSE 3000\n",
		"FROM golang:1.22 AS b\nFROM scratch\nCOPY --from=b /a /a\n",
		"FROM --platform=$BUILDPLATFORM a AS a\nFROM a\n",
		"RUN <<EOF\nFROM evil\nEOF\n",
		"ENV A=b C=\"d e\"\nENV F g\n",
		"ARG PORT=80\nENV PORT $PORT\nEXPOSE $PORT\n",
		"FROM\n", "", "#\n", "\\\n", "FROM a AS a\n", "EXPOSE ${}\n",
		"ENV URL=postgres://h:5432/d\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, content string) {
		c := &capture{}
		file := &scan.File{
			FileMeta: scan.FileMeta{
				Path: "svc/Dockerfile", Dir: "svc", Name: "Dockerfile",
				Size: int64(len(content)),
			},
			Content: []byte(content),
		}
		if err := dockerfile.New().Extract(context.Background(), file, c); err != nil {
			t.Fatalf("Extract() = %v", err)
		}
		// Every emitted node has to be identifiable, or it is a box on the
		// diagram that nothing can refer to and nobody can place.
		for _, n := range c.nodes {
			if n.Name == "" {
				t.Fatalf("a component with no name: %+v", n)
			}
			if n.ID == "" {
				t.Fatalf("a component with no identifier: %+v", n)
			}
			if !n.Kind.Valid() {
				t.Fatalf("Kind = %q, which is not a valid node kind", n.Kind)
			}
			if ports, ok := n.Attrs["ports"].([]int); ok {
				for _, p := range ports {
					if p <= 0 || p > 65535 {
						t.Fatalf("port %d is not a port", p)
					}
				}
			}
		}
		// A hint with an empty token matches every name in the repository.
		for _, h := range c.hints {
			if len(h.Tokens) == 0 {
				t.Fatalf("a reference with no tokens: %+v", h)
			}
			for _, tok := range h.Tokens {
				if strings.TrimSpace(tok) == "" {
					t.Fatalf("an empty reference token: %+v", h)
				}
			}
			if h.FromNode == "" {
				t.Fatalf("a reference attached to nothing: %+v", h)
			}
		}
	})
}
