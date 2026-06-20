package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMapGenerator(t *testing.T) {
	// Create a temp directory to put test files in mimicking a project
	tempDir, err := os.MkdirTemp("", "cove-repomap-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a test Go file inside the temp directory
	testGoCode := `package testpkg

import "context"

type MyStruct struct {
	Field string
}

type MyInterface interface {
	DoSomething(ctx context.Context, value string) error
}

func NewMyStruct(val string) *MyStruct {
	return &MyStruct{Field: val}
}

func (s *MyStruct) GetField() string {
	return s.Field
}
`
	goFilePath := filepath.Join(tempDir, "test.go")
	if err := os.WriteFile(goFilePath, []byte(testGoCode), 0644); err != nil {
		t.Fatalf("failed to write test go file: %v", err)
	}

	// Create a test Python file
	testPyCode := `class PythonAgent:
    def __init__(self, name):
        self.name = name

    def execute_task(self, query):
        pass
`
	pyFilePath := filepath.Join(tempDir, "agent.py")
	if err := os.WriteFile(pyFilePath, []byte(testPyCode), 0644); err != nil {
		t.Fatalf("failed to write test py file: %v", err)
	}

	// Create a test TS file
	testTsCode := `export interface Config {
    port: number;
}
export class Server {
    start() {
        console.log("running");
    }
}
`
	tsFilePath := filepath.Join(tempDir, "server.ts")
	if err := os.WriteFile(tsFilePath, []byte(testTsCode), 0644); err != nil {
		t.Fatalf("failed to write test ts file: %v", err)
	}

	gen := NewGenerator(tempDir)
	result := gen.Generate(10)

	// Check if all classes and methods are mapped nicely
	if !strings.Contains(result, "type MyStruct struct") {
		t.Errorf("expected MyStruct definition, got:\n%s", result)
	}
	if !strings.Contains(result, "type MyInterface interface") {
		t.Errorf("expected MyInterface definition, got:\n%s", result)
	}
	if !strings.Contains(result, "func NewMyStruct(val string)") {
		t.Errorf("expected NewMyStruct signature, got:\n%s", result)
	}
	if !strings.Contains(result, "func (s *MyStruct) GetField()") {
		t.Errorf("expected GetField method signature, got:\n%s", result)
	}

	// Python checks
	if !strings.Contains(result, "class PythonAgent") {
		t.Errorf("expected class PythonAgent definition, got:\n%s", result)
	}
	if !strings.Contains(result, "def execute_task") {
		t.Errorf("expected def execute_task, got:\n%s", result)
	}

	// TS checks
	if !strings.Contains(result, "interface Config") {
		t.Errorf("expected interface Config, got:\n%s", result)
	}
	if !strings.Contains(result, "class Server") {
		t.Errorf("expected class Server, got:\n%s", result)
	}
}
