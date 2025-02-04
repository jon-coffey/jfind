package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultPostURL = "http://localhost:8080/jfind"
)

// JavaFinder represents a finder for Java executables
type JavaFinder struct {
	startPath string
	maxDepth  int // -1 means unlimited
	verbose   bool
	evaluate  bool
	scanned   int
}

// JavaResult represents the result of evaluating a Java executable
type JavaResult struct {
	Path       string
	Properties *JavaProperties
	Warnings   []string
	StdErr     string
	ReturnCode int
	Error      error
}

// JavaRuntimeJSON represents a single Java runtime for JSON output
type JavaRuntimeJSON struct {
	JavaExecutable string `json:"java.executable"`
	JavaVersion    string `json:"java.version,omitempty"`
	JavaVendor     string `json:"java.vendor,omitempty"`
	JavaRuntime    string `json:"java.runtime.name,omitempty"`
	IsOracle       bool   `json:"is_oracle,omitempty"`
}

// MetaInfo represents metadata about the scan
type MetaInfo struct {
	ScanTimestamp string `json:"scan_ts"`
	ComputerName  string `json:"computer_name"`
	UserName      string `json:"user_name"`
	ScanDuration  string `json:"scan_duration"`
	HasOracleJDK  bool   `json:"has_oracle_jdk"`
	CountResult   int    `json:"count_result"`
	ScannedDirs   int    `json:"scanned_dirs"`
}

// JSONOutput represents the root JSON output structure
type JSONOutput struct {
	Meta     MetaInfo          `json:"meta"`
	Runtimes []JavaRuntimeJSON `json:"result"`
}

// NewJavaFinder creates a new JavaFinder instance
func NewJavaFinder(startPath string, maxDepth int, verbose bool, evaluate bool) *JavaFinder {
	return &JavaFinder{
		startPath: startPath,
		maxDepth:  maxDepth,
		verbose:   verbose,
		evaluate:  evaluate,
	}
}

// logf prints to stderr
func logf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format, a...)
}

// printf prints to stdout
func printf(format string, a ...interface{}) {
	fmt.Printf(format, a...)
}

// isExecutable checks if a file is executable based on the operating system
func isExecutable(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		// On Windows, we only check if it's a regular file
		return !info.IsDir()
	}
	// On Unix-like systems, check for executable permission
	return info.Mode()&0111 != 0
}

// isJavaExecutable checks if the filename matches java executable patterns
func isJavaExecutable(name string) bool {
	if runtime.GOOS == "windows" {
		return name == "java.exe"
	}
	return name == "java"
}

// getPathDepth returns the depth of a path relative to the start path
func (f *JavaFinder) getPathDepth(path string) int {
	relPath, err := filepath.Rel(f.startPath, path)
	if err != nil {
		return 0
	}
	if relPath == "." {
		return 0
	}
	return len(strings.Split(relPath, string(os.PathSeparator)))
}

// evaluateJava runs java -version and returns the result
func (f *JavaFinder) evaluateJava(javaPath string) JavaResult {
	result := JavaResult{
		Path: javaPath,
	}

	cmd := exec.Command(javaPath, "-XshowSettings:properties", "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			result.ReturnCode = exitError.ExitCode()
		}
		result.Error = err
	} else {
		result.ReturnCode = 0
	}

	// Java outputs properties and version info to stderr
	result.StdErr = stderr.String()
	result.Properties = ParseJavaProperties(stderr.String())

	// Check for Oracle vendor
	if result.Properties != nil && strings.Contains(result.Properties.Vendor, "Oracle") {
		result.Warnings = append(result.Warnings, "Warning: Oracle vendor detected")
	}

	return result
}

// printResult prints the results of evaluating a Java executable
func printResult(result *JavaResult) {
	if result.Error != nil {
		printf("Failed to execute: %v\n", result.Error)
		return
	}

	if result.ReturnCode != 0 {
		printf("Command failed with return code: %d\n", result.ReturnCode)
		return
	}

	printf("Java executable: %s\n", result.Path)

	if result.Properties != nil {
		printf("Java version: %s\n", result.Properties.Version)
		printf("Java vendor: %s\n", result.Properties.Vendor)
		if result.Properties.RuntimeName != "" {
			printf("Java runtime name: %s\n", result.Properties.RuntimeName)
		}
	}

	if len(result.Warnings) > 0 {
		for _, warning := range result.Warnings {
			printf("%s\n", warning)
		}
	}
}

// Find searches for java executables starting from the specified path
func (f *JavaFinder) Find() ([]*JavaResult, error) {
	f.scanned = 0 // Reset counter
	if f.verbose {
		logf("Start looking for java in %s (scanning subdirectories)\n", f.startPath)
	}
	var results []*JavaResult

	err := filepath.Walk(f.startPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				if f.verbose {
					logf("Permission denied: %s\n", path)
				}
				return filepath.SkipDir
			}
			// Skip other errors but log them in verbose mode
			if f.verbose {
				logf("Error accessing %s: %v\n", path, err)
			}
			return nil
		}

		// Print directory being scanned in verbose mode
		if f.verbose && info.IsDir() {
			logf("Scanning: %s\n", path)
		}

		// Count directories as we scan
		if info.IsDir() {
			f.scanned++
		}

		// Check depth
		if f.maxDepth >= 0 && f.getPathDepth(path) > f.maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Check if file is executable and matches java pattern
		if !info.IsDir() && isExecutable(info) && isJavaExecutable(filepath.Base(path)) {
			// Always log the executable path to stderr when found
			logf("%s\n", path)

			if f.evaluate {
				result := f.evaluateJava(path)
				results = append(results, &result)
			} else {
				// For non-evaluated executables, create a basic result
				result := JavaResult{
					Path: path,
				}
				results = append(results, &result)
			}
		}
		return nil
	})

	return results, err
}

// formatDurationISO8601 formats a duration according to ISO8601 with millisecond precision
func formatDurationISO8601(d time.Duration) string {
	d = d.Round(time.Millisecond)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	d -= s * time.Second
	ms := d / time.Millisecond

	var result strings.Builder
	result.WriteString("PT")
	if h > 0 {
		result.WriteString(fmt.Sprintf("%dH", h))
	}
	if m > 0 {
		result.WriteString(fmt.Sprintf("%dM", m))
	}
	if s > 0 || ms > 0 || (h == 0 && m == 0) {
		if ms > 0 {
			result.WriteString(fmt.Sprintf("%d.%03dS", s, ms))
		} else {
			result.WriteString(fmt.Sprintf("%dS", s))
		}
	}
	return result.String()
}

func getComputerName() string {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("scutil", "--get", "ComputerName")
		output, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(output))
		}
	case "windows":
		cmd := exec.Command("cmd", "/c", "hostname")
		output, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(output))
		}
	case "linux":
		// Try to read from /etc/hostname first
		if data, err := os.ReadFile("/etc/hostname"); err == nil {
			return strings.TrimSpace(string(data))
		}
		// Fallback to hostname command
		cmd := exec.Command("hostname")
		output, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(output))
		}
	}
	return "unknown"
}

// sendJSON sends the JSON payload to the specified URL via HTTP POST
func sendJSON(jsonData []byte, url string) error {
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		// Check if it's a connection error
		if netErr, ok := err.(*net.OpError); ok {
			return fmt.Errorf("failed to connect to server at %s: %v", url, netErr)
		}
		return fmt.Errorf("failed to send JSON to %s: %v", url, err)
	}
	defer resp.Body.Close()

	// Read response body for error details
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if len(body) > 0 {
			return fmt.Errorf("server returned %s: %s", resp.Status, string(body))
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}

	return nil
}

func main() {
	var startPath string
	var maxDepth int
	var verbose bool
	var evaluate bool
	var jsonOutput bool
	var doPost bool

	flag.StringVar(&startPath, "path", ".", "Start path for searching")
	flag.IntVar(&maxDepth, "depth", -1, "Maximum depth to search (-1 for unlimited)")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose output")
	flag.BoolVar(&evaluate, "eval", false, "Evaluate found java executables")
	flag.BoolVar(&jsonOutput, "json", false, "Output results in JSON format")
	flag.BoolVar(&doPost, "post", false, "Post JSON output to server (implies --json)")
	flag.Parse()

	// Get optional URL from remaining args or use default
	var postURL string
	args := flag.Args()
	if doPost {
		jsonOutput = true
		if len(args) > 0 {
			postURL = args[0]
		} else {
			postURL = defaultPostURL
		}
	}

	// Convert relative path to absolute
	absPath, err := filepath.Abs(startPath)
	if err != nil {
		logf("Error resolving path: %v\n", err)
		os.Exit(1)
	}

	finder := NewJavaFinder(absPath, maxDepth, verbose, evaluate)
	startTime := time.Now()
	results, err := finder.Find()
	if err != nil {
		logf("Error during search: %v\n", err)
		os.Exit(1)
	}

	if jsonOutput {
		// Get meta information
		currentUser, _ := user.Current()
		username := "unknown"
		if currentUser != nil {
			username = currentUser.Username
		}

		hasOracle := false
		duration := formatDurationISO8601(time.Since(startTime))
		output := JSONOutput{
			Meta: MetaInfo{
				ScanTimestamp: time.Now().UTC().Format(time.RFC3339),
				ComputerName:  getComputerName(),
				UserName:      username,
				ScanDuration:  duration,
				HasOracleJDK:  false,
				CountResult:   len(results),
				ScannedDirs:   finder.scanned,
			},
			Runtimes: make([]JavaRuntimeJSON, 0),
		}

		for _, result := range results {
			runtime := JavaRuntimeJSON{
				JavaExecutable: result.Path,
			}

			if evaluate && result.Properties != nil && result.Error == nil && result.ReturnCode == 0 {
				runtime.JavaVersion = result.Properties.Version
				runtime.JavaVendor = result.Properties.Vendor
				runtime.JavaRuntime = result.Properties.RuntimeName
				runtime.IsOracle = strings.Contains(result.Properties.Vendor, "Oracle")
				if runtime.IsOracle {
					hasOracle = true
				}
			}

			output.Runtimes = append(output.Runtimes, runtime)
		}

		// Update hasOracle after scanning all results
		output.Meta.HasOracleJDK = hasOracle

		jsonData, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			logf("Error generating JSON output: %v\n", err)
			os.Exit(1)
		}

		if doPost {
			logf("Posting JSON to %s...\n", postURL)
			if err := sendJSON(jsonData, postURL); err != nil {
				logf("Error: %v\n", err)
				os.Exit(1)
			}
			logf("Successfully posted JSON to %s\n", postURL)
		} else {
			fmt.Println(string(jsonData))
		}
	} else {
		for _, result := range results {
			printResult(result)
			printf("\n")
		}
	}
}
