package agent

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
)

type Bootstrap struct {
	// If the bootstrap is in debug mode
	Debug bool

	// Slug of the current pipeline
	PipelineSlug string

	// Name of the agent running the bootstrap
	AgentName string

	// Should the bootstrap remove an existing checkout before running the job
	CleanCheckout bool

	// Whether or not to run the hooks/commands in a PTY
	RunInPty bool

	// Path where the builds will be run
	BuildPath string

	// Path to the buildkite-agent binary
	BinPath string

	// Path to the global hooks
	HooksPath string

	// The running environment for the bootstrap file as each task runs
	env map[string]string
}

var agentNameCleanupRegex = regexp.MustCompile("\"")

// Prints a line of output
func printf(format string, v ...interface{}) {
	fmt.Printf("%s\n", fmt.Sprintf(format, v...))
}

// Prints a bootstrap formatted header
func headerf(format string, v ...interface{}) {
	fmt.Printf("~~~ %s\n", fmt.Sprintf(format, v...))
}

// Shows the error text and exits the bootstrap
func fatalf(format string, v ...interface{}) {
	headerf(":rotating_light: \033[31mBuildkite Error\033[0m")
	printf(format, v...)
	os.Exit(1)
}

// Returns whether or not a file exists on the filesystem
func fileExists(filename string) bool {
	_, err := os.Stat(filename)

	return !os.IsNotExist(err)
}

// Returns the current working directroy
func getWorkingDirectory() string {
	wd, _ := os.Getwd()

	return wd
}

// Reads a file into an ENV map
func readEnvFileIntoMap(filename string) map[string]string {
	env := make(map[string]string)

	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		fatalf("Error reading file: %s", err)
	}

	lines := strings.Split(string(contents), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	return env
}

// Turns a ENV map into a K=V slice
func convertEnvMapIntoSlice(env map[string]string) []string {
	slice := []string{}
	for k, v := range env {
		slice = append(slice, fmt.Sprintf("%s=%s", k, v))
	}
	return slice
}

// Comapres 2 env maps and returns the diff
func diffEnvMaps(beforeEnv map[string]string, afterEnv map[string]string) map[string]string {
	diff := make(map[string]string)

	for afterEnvKey, afterEnvValue := range afterEnv {
		if beforeEnv[afterEnvKey] != afterEnvValue {
			diff[afterEnvKey] = afterEnvValue
		}
	}

	return diff
}

// Executes a hook and applyes any environment changes. The tricky thing with
// hooks is that they can modify the ENV of a bootstrap. And it's impossible to
// grab the ENV of a child process before it finishes, so we've got an awesome
// ugly hack to get around this.  We essentially have a bash script that writes
// the ENV to a file, runs the hook, then writes the ENV back to another file.
// Once all that has finished, we compare the files, and apply what ever
// changes to our running env. Cool huh?
func (b Bootstrap) executeHook(name string, path string) {
	// Check if the hook exists
	if fileExists(path) {
		// Create a temporary file that we'll put the hook runner code in
		tempHookRunnerFile, err := ioutil.TempFile("", "buildkite-agent-bootstrap-hook-runner")
		defer tempHookRunnerFile.Close()

		// Mark the temporary hook runner file as writable
		s, err := os.Stat(tempHookRunnerFile.Name())
		if err != nil {
			fatalf("Failed to retrieve file information of `%s` as executable (%s)", tempHookRunnerFile.Name(), err)
		}
		err = os.Chmod(tempHookRunnerFile.Name(), s.Mode()|0100)
		if err != nil {
			fatalf("Failed to mark `%s` as executable (%s)", tempHookRunnerFile.Name(), err)
		}

		// We'll pump the ENV before the hook into this temp file
		tempEnvBeforeFile, err := ioutil.TempFile("", "buildkite-agent-bootstrap-hook-env-before")
		defer tempEnvBeforeFile.Close()

		// We'll then pump the ENV _after_ the hook into this temp file
		tempEnvAfterFile, err := ioutil.TempFile("", "buildkite-agent-bootstrap-hook-env-after")
		defer tempEnvAfterFile.Close()

		// Create the hook runner code
		var hookScript string
		if runtime.GOOS == "windows" {
			hookScript = "SET > \"" + tempEnvBeforeFile.Name() + "\"\n" +
				"call \"" + path + "\"\n" +
				"BUILDKITE_LAST_HOOK_EXIT_STATUS=!ERRORLEVEL!\n" +
				"SET > \"" + tempEnvAfterFile.Name() + "\""
		} else {
			hookScript = "#!/bin/bash\n" +
				"env > \"" + tempEnvBeforeFile.Name() + "\"\n" +
				". \"" + path + "\"\n" +
				"export BUILDKITE_LAST_HOOK_EXIT_STATUS=$?\n" +
				"env > \"" + tempEnvAfterFile.Name() + "\""
		}

		// Write the hook script to the runner
		tempHookRunnerFile.WriteString(hookScript)
		tempHookRunnerFile.Sync()

		// Print to the screen we're going to run the hook
		headerf("Running %s hook", path)

		// Run the hook
		b.shell(b.env, tempHookRunnerFile.Name())

		// Compare the ENV current env with the after shots, then
		// modify the running env map with the changes.
		envDiff := diffEnvMaps(readEnvFileIntoMap(tempEnvBeforeFile.Name()), readEnvFileIntoMap(tempEnvAfterFile.Name()))
		if len(envDiff) > 0 {
			if b.Debug {
				headerf("Applying environment changes")
			}
			for envDiffKey, envDiffValue := range envDiff {
				b.env[envDiffKey] = envDiffValue
				if b.Debug {
					printf("%s=%s was added/changed to the environment", envDiffKey, envDiffValue)
				}
			}
		}
	} else {
		if b.Debug {
			headerf("Running %s hook", name)
			printf("Skipping, no hook script found at: %s", path)
		}
	}
}

func (b Bootstrap) executeGlobalHook(name string) {
	b.executeHook("global "+name, path.Join(b.HooksPath, name))
}

func (b Bootstrap) executeLocalHook(name string) {
	b.executeHook("local "+name, path.Join(getWorkingDirectory(), ".buildkite", "hooks", name))
}

func (b Bootstrap) Start() error {
	// Create an empty env for us to keep track of our env changes in
	b.env = make(map[string]string)

	// Add the $BUILDKITE_BIN_PATH to the $PATH
	b.env["PATH"] = fmt.Sprintf("%s:%s", b.BinPath, os.Getenv("PATH"))

	// Come up with the place that the repository will be checked out to
	cleanedUpAgentName := agentNameCleanupRegex.ReplaceAllString(b.AgentName, "-")
	b.env["BUILDKITE_BUILD_CHECKOUT_PATH"] = path.Join(b.BuildPath, cleanedUpAgentName, b.PipelineSlug)

	// $ SANITIZED_AGENT_NAME=$(echo "$BUILDKITE_AGENT_NAME" | tr -d '"')
	// $ PROJECT_FOLDER_NAME="$SANITIZED_AGENT_NAME/$BUILDKITE_PROJECT_SLUG"
	// $ export BUILDKITE_BUILD_CHECKOUT_PATH="$BUILDKITE_BUILD_PATH/$PROJECT_FOLDER_NAME"

	// Show BUILDKITE_* environment variables if in debug mode. Also
	// include any custom BUILDKITE_ variables that have been added to our
	// running env map.
	if b.Debug {
		headerf("Build environment variables")
		for _, e := range append(convertEnvMapIntoSlice(b.env), os.Environ()...) {
			if strings.HasPrefix(e, "BUILDKITE") {
				printf(e)
			}
		}
	}

	//////////////////////////////////////////////////////////////
	//
	// ENVIRONMENT SETUP
	// A place for people to set up environment variables that
	// might be needed for their build scripts, such as secret
	// tokens and other information.
	//
	//////////////////////////////////////////////////////////////

	// The global environment hook
	b.executeGlobalHook("environment")

	//////////////////////////////////////////////////////////////
	//
	// REPOSITORY HANDLING
	// Creates the build folder and makes sure we're running the
	// build at the right commit.
	//
	//////////////////////////////////////////////////////////////

	// Run the `pre-checkout` global hook
	b.executeGlobalHook("pre-checkout")

	// Remove the checkout folder if BUILDKITE_CLEAN_CHECKOUT is present
	if b.CleanCheckout {
		headerf("Cleaning project checkout")
		printf("Removing %s", b.env["BUILDKITE_BUILD_CHECKOUT_PATH"])

		err := os.RemoveAll(b.env["BUILDKITE_BUILD_CHECKOUT_PATH"])
		if err != nil {
			fatalf("Failed to remove `%s` (%s)", b.env["BUILDKITE_BUILD_CHECKOUT_PATH"], err)
		}
	}

	// Run the `post-checkout` global hook
	b.executeGlobalHook("post-checkout")

	// Run the `post-checkout` local hook
	b.executeLocalHook("post-checkout")

	return nil
}
