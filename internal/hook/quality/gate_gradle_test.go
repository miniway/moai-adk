package quality

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestQualityGate_detectToolchain_GradleVsMaven pins the Java/Kotlin toolchain
// split that stops every Gradle project routing to Maven.
//
// Before the split, one conflated Java entry claimed pom.xml, build.gradle AND
// build.gradle.kts at once, and detectToolchain is first-match-wins with the
// Java entry ordered first, so EVERY Gradle project resolved to `mvn test` and
// failed with MissingProjectException in a repository that has no pom.xml
// anywhere. `optional: true` did not save it: optional only skips when the
// BINARY is missing, and mvn is commonly installed.
func TestQualityGate_detectToolchain_GradleVsMaven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// files laid out in the fixture project dir.
		files map[string]string
		// wantMarker is the first markerFile of the expected toolchain entry.
		wantMarker string
		// wantTestName is the test step name, stable across wrapper resolution.
		wantTestName string
		// wantWrapperBinary asserts the test binary is the project-local Gradle
		// wrapper rather than a bare binary resolved off $PATH.
		wantWrapperBinary bool
		// wantTestBinary is the expected binary when wantWrapperBinary is false.
		wantTestBinary string
		// wantTestOptional is the expected optional flag on the test step.
		wantTestOptional bool
		// wantLintName is the expected first lint step name.
		wantLintName string
	}{
		{
			name: "pure Kotlin gradle project with wrapper -> gradle wrapper, never mvn",
			files: map[string]string{
				"build.gradle.kts":        "plugins { id java }\n",
				"gradlew":                 "#!/bin/sh\n",
				"gradlew.bat":             "@echo off\n",
				"src/main/kotlin/Main.kt": "fun main() {}\n",
			},
			wantMarker:        "build.gradle.kts",
			wantTestName:      "gradle test",
			wantWrapperBinary: true,
			wantTestOptional:  false,
			wantLintName:      "ktlint",
		},
		{
			name:             "maven only -> mvn test",
			files:            map[string]string{"pom.xml": "<project/>\n"},
			wantMarker:       "pom.xml",
			wantTestName:     "mvn test",
			wantTestBinary:   "mvn",
			wantTestOptional: true,
			wantLintName:     "checkstyle",
		},
		{
			name: "groovy gradle (build.gradle, no kts) with wrapper -> gradle wrapper + checkstyle",
			files: map[string]string{
				"build.gradle": "apply plugin: java\n",
				"gradlew":      "#!/bin/sh\n",
				"gradlew.bat":  "@echo off\n",
			},
			wantMarker:        "build.gradle",
			wantTestName:      "gradle test",
			wantWrapperBinary: true,
			wantTestOptional:  false,
			wantLintName:      "checkstyle",
		},
		{
			name: "pom.xml and build.gradle.kts both present -> Maven wins",
			files: map[string]string{
				"pom.xml":          "<project/>\n",
				"build.gradle.kts": "plugins { id java }\n",
				"gradlew":          "#!/bin/sh\n",
			},
			wantMarker:       "pom.xml",
			wantTestName:     "mvn test",
			wantTestBinary:   "mvn",
			wantTestOptional: true,
			wantLintName:     "checkstyle",
		},
		{
			name:             "kts without wrapper -> bare gradle, still optional",
			files:            map[string]string{"build.gradle.kts": "plugins { id java }\n"},
			wantMarker:       "build.gradle.kts",
			wantTestName:     "gradle test",
			wantTestBinary:   "gradle",
			wantTestOptional: true,
			wantLintName:     "ktlint",
		},
		{
			name:             "groovy gradle without wrapper -> bare gradle, still optional",
			files:            map[string]string{"build.gradle": "apply plugin: java\n"},
			wantMarker:       "build.gradle",
			wantTestName:     "gradle test",
			wantTestBinary:   "gradle",
			wantTestOptional: true,
			wantLintName:     "checkstyle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := writeFixture(t, tt.files)

			g := NewQualityGate(&GateConfig{ProjectDir: dir})
			tc := g.detectToolchain()
			if tc == nil {
				t.Fatalf("detectToolchain() = nil, want toolchain with marker %q", tt.wantMarker)
			}
			if got := tc.markerFiles[0]; got != tt.wantMarker {
				t.Errorf("first markerFile = %q, want %q", got, tt.wantMarker)
			}
			if tc.testStep == nil {
				t.Fatalf("testStep = nil, want %q", tt.wantTestName)
			}
			if got := tc.testStep.name; got != tt.wantTestName {
				t.Errorf("testStep.name = %q, want %q", got, tt.wantTestName)
			}
			if tt.wantWrapperBinary {
				want := filepath.Join(dir, "gradlew")
				if got := tc.testStep.binary; got != want {
					t.Errorf("testStep.binary = %q, want project wrapper %q", got, want)
				}
			} else if got := tc.testStep.binary; got != tt.wantTestBinary {
				t.Errorf("testStep.binary = %q, want %q", got, tt.wantTestBinary)
			}
			if got := tc.testStep.optional; got != tt.wantTestOptional {
				t.Errorf("testStep.optional = %v, want %v", got, tt.wantTestOptional)
			}
			if len(tc.lintSteps) == 0 {
				t.Fatalf("lintSteps empty, want first step %q", tt.wantLintName)
			}
			if got := tc.lintSteps[0].name; got != tt.wantLintName {
				t.Errorf("lintSteps[0].name = %q, want %q", got, tt.wantLintName)
			}
		})
	}
}

// TestQualityGate_detectToolchain_GradleStepNameStable pins the step NAME as a
// contract surface. disabled_steps in .moai/config/sections/gate.yaml is keyed
// by step name, so a name that varied with wrapper presence would silently
// break user config on any machine that happens to lack the wrapper.
func TestQualityGate_detectToolchain_GradleStepNameStable(t *testing.T) {
	t.Parallel()

	// gradleTestStepName IS the disabled_steps config key, so pin its literal
	// value: a rename would break user config silently rather than loudly.
	if gradleTestStepName != "gradle test" {
		t.Fatalf("gradleTestStepName = %q, want %q (disabled_steps config key)",
			gradleTestStepName, "gradle test")
	}

	cases := []struct {
		name        string
		files       map[string]string
		wantWrapper bool
	}{
		{
			name: "with wrapper",
			files: map[string]string{
				"build.gradle.kts": "plugins { id java }\n",
				"gradlew":          "#!/bin/sh\n",
			},
			wantWrapper: true,
		},
		{
			name:  "without wrapper",
			files: map[string]string{"build.gradle.kts": "plugins { id java }\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := writeFixture(t, tc.files)

			g := NewQualityGate(&GateConfig{ProjectDir: dir})
			got := g.detectToolchain()
			if got == nil || got.testStep == nil {
				t.Fatalf("detectToolchain() produced no test step")
			}
			if got.testStep.name != "gradle test" {
				t.Errorf("testStep.name = %q, want %q (the disabled_steps config key)",
					got.testStep.name, "gradle test")
			}
			// Sanity: the two cases really do differ in the resolved binary,
			// so the stable name above is not stable merely by accident.
			isWrapper := got.testStep.binary == filepath.Join(dir, "gradlew")
			if isWrapper != tc.wantWrapper {
				t.Errorf("wrapper resolution = %v, want %v (binary %q)",
					isWrapper, tc.wantWrapper, got.testStep.binary)
			}
		})
	}
}

// TestQualityGate_executeStep_GradleWrapperRunsWithEmptyPATH is the false-green
// guard.
//
// A bare `gradle` is absent from most developer machines, where the committed
// wrapper is the only Gradle present. An optional step whose binary misses
// LookPath is skipped SILENTLY, so a correctly detected Kotlin project used to
// have its test step dropped and the gate reported PASS having run no tests at
// all. That false green is worse than a loud failure, so the wrapper form must
// actually RUN even when $PATH resolves nothing.
func TestQualityGate_executeStep_GradleWrapperRunsWithEmptyPATH(t *testing.T) {
	// Deliberately not parallel: t.Setenv mutates process-wide state.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell wrapper fixture; skipping on windows")
	}

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "wrapper-ran")
	// The wrapper records that it ran, so a SKIP is distinguishable from a pass.
	script := "#!/bin/sh\necho ran > " + sentinel + "\n"
	wrapper := filepath.Join(dir, "gradlew")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte(""), 0o644); err != nil {
		t.Fatalf("write build.gradle.kts: %v", err)
	}

	// Empty PATH: a bare `gradle` is genuinely unresolvable.
	t.Setenv("PATH", "")
	if _, err := exec.LookPath("gradle"); err == nil {
		t.Fatal("precondition failed: gradle is still resolvable with an empty PATH")
	}

	g := NewQualityGate(&GateConfig{ProjectDir: dir, TestTimeout: 30 * time.Second})
	tc := g.detectToolchain()
	if tc == nil || tc.testStep == nil {
		t.Fatalf("detectToolchain() produced no test step for a Kotlin/Gradle fixture")
	}
	if tc.testStep.binary != wrapper {
		t.Fatalf("testStep.binary = %q, want the wrapper %q", tc.testStep.binary, wrapper)
	}

	passed, output := g.executeStep(context.Background(), *tc.testStep, 30*time.Second)
	if !passed {
		t.Fatalf("executeStep() = false, want true; output: %q", output)
	}
	// The load-bearing assertion: the step RAN. Without it this test would pass
	// on the silent-skip path it exists to forbid.
	if !fileExists(sentinel) {
		t.Errorf("gradle wrapper step was SKIPPED (no sentinel at %s): false green", sentinel)
	}
}
