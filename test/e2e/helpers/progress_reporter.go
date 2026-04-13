/*
Copyright 2026 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

const (
	// DefaultReportDirName is the default directory name for test reports
	DefaultReportDirName = "Reports"
	// DefaultProgressReportFileName is the default filename for progress reports
	DefaultProgressReportFileName = "progress_report.txt"
)

// ProgressReporter tracks and reports test progress
type ProgressReporter struct {
	startTime  time.Time
	testCount  int
	passCount  int
	failCount  int
	skipCount  int
	testsTotal int
	reportFile string
}

// NewProgressReporter creates a new progress reporter
func NewProgressReporter(reportDir string) *ProgressReporter {
	pr := &ProgressReporter{
		startTime: time.Now(),
	}

	// Ensure reportDir is an absolute path
	if reportDir == "" {
		reportDir = DefaultReportDirName
	}

	if !filepath.IsAbs(reportDir) {
		// If relative, make it absolute from current working directory
		wd, err := os.Getwd()
		if err == nil {
			reportDir = filepath.Join(wd, reportDir)
		}
	}

	// Create the directory if it doesn't exist
	if err := os.MkdirAll(reportDir, 0755); err != nil {
		fmt.Printf("[PROGRESS] Error creating report directory %s: %v\n", reportDir, err)
	}

	// Set up report file path
	pr.reportFile = filepath.Join(reportDir, DefaultProgressReportFileName)

	return pr
}

// SetStartMsg logs the start of the test suite
func (pr *ProgressReporter) SetStartMsg(suiteName string) {
	msg := fmt.Sprintf("=== %s Started at %s ===\n", suiteName, pr.startTime.Format(time.RFC3339))
	pr.writeToReport(msg)
}

// SetEndMsg logs the end of the test suite
func (pr *ProgressReporter) SetEndMsg(suiteName string) {
	elapsed := time.Since(pr.startTime)
	msg := fmt.Sprintf("\n=== %s Completed ===\n"+
		"Duration: %v\n"+
		"Total Tests: %d\n"+
		"Passed: %d\n"+
		"Failed: %d\n"+
		"Skipped: %d\n"+
		"End Time: %s\n",
		suiteName, elapsed, pr.testCount, pr.passCount, pr.failCount, pr.skipCount,
		time.Now().Format(time.RFC3339))
	pr.writeToReport(msg)
}

// AddProgress records test progress
func (pr *ProgressReporter) AddProgress(testName string, status string) {
	msg := fmt.Sprintf("[%s] %s - %s\n", time.Now().Format("15:04:05"), testName, status)
	pr.writeToReport(msg)

	pr.testCount++
	switch status {
	case "PASS":
		pr.passCount++
	case "FAIL":
		pr.failCount++
	case "SKIP":
		pr.skipCount++
	}
}

// SetTestsTotal sets the total number of tests that will run
func (pr *ProgressReporter) SetTestsTotal(total int) {
	pr.testsTotal = total
	msg := fmt.Sprintf("Total tests to run: %d\n", total)
	pr.writeToReport(msg)
}

// ProcessSpecReport processes individual spec reports for real-time progress tracking
func (pr *ProgressReporter) ProcessSpecReport(report types.SpecReport) {
	// Skip lifecycle hooks and internal specs
	fullText := strings.TrimSpace(report.FullText())
	if fullText == "" ||
		strings.Contains(fullText, "[BeforeSuite]") ||
		strings.Contains(fullText, "[AfterSuite]") ||
		strings.Contains(fullText, "[BeforeEach]") ||
		strings.Contains(fullText, "[AfterEach]") ||
		strings.HasPrefix(fullText, "TOP-LEVEL") {
		return
	}

	// Determine status
	var status string
	switch report.State {
	case types.SpecStatePassed:
		status = "PASS"
		pr.passCount++
	case types.SpecStateSkipped, types.SpecStatePending:
		status = "SKIP"
		pr.skipCount++
	default:
		if report.Failed() {
			status = "FAIL"
			pr.failCount++
		} else {
			return
		}
	}

	pr.testCount++

	// Log progress
	duration := report.RunTime.Round(time.Millisecond)
	msg := fmt.Sprintf("[%s] %s - %s (%s)\n", time.Now().Format("15:04:05"), fullText, status, duration)
	pr.writeToReport(msg)
}

// writeToReport writes progress to report file if configured
func (pr *ProgressReporter) writeToReport(msg string) {
	if pr.reportFile == "" {
		return
	}

	// Append to report file (file should already exist from NewProgressReporter)
	f, err := os.OpenFile(pr.reportFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("[PROGRESS] Warning: could not open report file %s: %v\n", pr.reportFile, err)
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(msg); err != nil {
		fmt.Printf("[PROGRESS] Warning: could not write to report file %s: %v\n", pr.reportFile, err)
	}
}
