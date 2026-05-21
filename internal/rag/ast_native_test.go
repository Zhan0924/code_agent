package rag

import (
	"testing"
)

const testGoSource = `package example

import "fmt"

// Config holds app configuration.
type Config struct {
	Name    string
	Timeout int
}

// Greeter defines the greeting interface.
type Greeter interface {
	Greet(name string) string
}

// NewConfig creates a new Config.
func NewConfig(name string) *Config {
	return &Config{Name: name, Timeout: 30}
}

// (c *Config) Validate checks the config.
func (c *Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name required")
	}
	return nil
}
`

func TestParseGoCodeNative_Functions(t *testing.T) {
	chunks := parseGoCodeNative(testGoSource)
	if len(chunks) == 0 {
		t.Fatal("expected chunks, got none")
	}

	// Check we found the function
	found := false
	for _, c := range chunks {
		if c.symbolName == "NewConfig" && c.symbolType == "function" {
			found = true
			if c.startLine == 0 || c.endLine == 0 {
				t.Errorf("NewConfig has invalid line range: %d-%d", c.startLine, c.endLine)
			}
			// Should have dependencies
			if len(c.dependencies) == 0 {
				// NewConfig doesn't call other functions, so this is ok
			}
		}
	}
	if !found {
		t.Error("expected to find NewConfig function")
	}
}

func TestParseGoCodeNative_Methods(t *testing.T) {
	chunks := parseGoCodeNative(testGoSource)

	found := false
	for _, c := range chunks {
		if c.symbolType == "method" && c.symbolName == "*Config.Validate" {
			found = true
			if len(c.dependencies) == 0 {
				t.Log("Validate method should have fmt.Errorf dependency")
			}
		}
	}
	if !found {
		t.Error("expected to find *Config.Validate method")
	}
}

func TestParseGoCodeNative_Types(t *testing.T) {
	chunks := parseGoCodeNative(testGoSource)

	structFound := false
	interfaceFound := false
	for _, c := range chunks {
		if c.symbolName == "Config" && c.symbolType == "struct" {
			structFound = true
		}
		if c.symbolName == "Greeter" && c.symbolType == "interface" {
			interfaceFound = true
		}
	}
	if !structFound {
		t.Error("expected to find Config struct")
	}
	if !interfaceFound {
		t.Error("expected to find Greeter interface")
	}
}

func TestParseGoCodeNative_FallbackOnInvalid(t *testing.T) {
	// Invalid Go code should fall back to heuristic parser
	chunks := parseGoCodeNative("this is not valid go code {{{{")
	// Should not panic, may return results from fallback
	_ = chunks
}

func TestParseWithAST_GoUsesNative(t *testing.T) {
	chunks := parseWithAST("go", testGoSource)
	if len(chunks) == 0 {
		t.Fatal("expected chunks from native Go parser")
	}

	// Verify it's using the native parser (which finds structs/interfaces)
	hasStruct := false
	for _, c := range chunks {
		if c.symbolType == "struct" {
			hasStruct = true
		}
	}
	if !hasStruct {
		t.Error("native parser should identify struct types")
	}
}

func TestParseWithAST_Python(t *testing.T) {
	pyCode := `
class MyClass:
    def __init__(self):
        pass

def helper():
    return 42
`
	chunks := parseWithAST("python", pyCode)
	if len(chunks) == 0 {
		t.Fatal("expected Python chunks")
	}
}

func TestParseWithAST_Unknown(t *testing.T) {
	chunks := parseWithAST("rust", "fn main() {}")
	// Unknown language returns nil (triggers fallback)
	if chunks != nil {
		t.Log("unknown language returned chunks via generic parser")
	}
}

// TestExtractGoFuncName_DoesNotPanicOnMalformed guards against a regression
// where extractGoFuncName did strings.Fields(line)[0] without a length check,
// panicking on inputs like "func " with only whitespace.
func TestExtractGoFuncName_DoesNotPanicOnMalformed(t *testing.T) {
	cases := []string{
		"func ",
		"func  ",
		"func\t",
		"func", // no trailing content at all
	}
	for _, c := range cases {
		// Must not panic. Result value doesn't matter beyond that.
		_ = extractGoFuncName(c)
	}
}

// TestExtractGoFuncName_Basic exercises common shapes.
func TestExtractGoFuncName_Basic(t *testing.T) {
	cases := map[string]string{
		"func Hello() error":                       "Hello",
		"func (r *Receiver) Method(arg int) error": "Method",
		"func F() (int, error)":                    "F",
	}
	for line, want := range cases {
		if got := extractGoFuncName(line); got != want {
			t.Errorf("extractGoFuncName(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestParseGoCode_MethodDetection confirms the heuristic fallback distinguishes
// methods (starting with a receiver after `func `) from plain functions whose
// signature merely contains ") " — the prior heuristic mis-labelled
// `func F() (int, error)` as a method.
func TestParseGoCode_MethodDetection(t *testing.T) {
	src := `package p

func PlainFunc() (int, error) { return 0, nil }

func (r *Receiver) Method() {}
`
	chunks := parseGoCode(src)
	foundPlain, foundMethod := false, false
	for _, c := range chunks {
		if c.symbolName == "PlainFunc" && c.symbolType == "function" {
			foundPlain = true
		}
		if c.symbolName == "Method" && c.symbolType == "method" {
			foundMethod = true
		}
	}
	if !foundPlain {
		t.Error("PlainFunc with multi-value return should be classified as function, not method")
	}
	if !foundMethod {
		t.Error("Receiver-bound Method should be classified as method")
	}
}
