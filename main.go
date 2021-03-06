package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bitrise-io/go-utils/command"
	"github.com/bitrise-io/go-utils/fileutil"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-tools/go-steputils/cache"
	"github.com/bitrise-tools/go-steputils/input"
	shellquote "github.com/kballard/go-shellquote"
)

// ConfigsModel ...
type ConfigsModel struct {
	GradleFile    string
	UnitTestTasks string
	GradlewPath   string
	UnitTestFlags string
	DeployDir     string
	CacheLevel    string
}

func createConfigsModelFromEnvs() ConfigsModel {
	return ConfigsModel{
		GradleFile:    os.Getenv("gradle_file"),
		UnitTestTasks: os.Getenv("unit_test_task"),
		GradlewPath:   os.Getenv("gradlew_file_path"),
		UnitTestFlags: os.Getenv("unit_test_flags"),
		DeployDir:     os.Getenv("BITRISE_DEPLOY_DIR"),
		CacheLevel:    os.Getenv("cache_level"),
	}
}

func (configs ConfigsModel) print() {
	log.Infof("Configs:")
	log.Printf("- GradleFile: %s", configs.GradleFile)
	log.Printf("- UnitTestTasks: %s", configs.UnitTestTasks)
	log.Printf("- GradlewPath: %s", configs.GradlewPath)
	log.Printf("- UnitTestFlags: %s", configs.UnitTestFlags)

	log.Printf("- DeployDir: %s", configs.DeployDir)
	log.Printf("- CacheLevel: %s", configs.CacheLevel)
}

func (configs ConfigsModel) validate() (string, error) {
	if configs.GradleFile != "" {
		if exist, err := pathutil.IsPathExists(configs.GradleFile); err != nil {
			return "", fmt.Errorf("Failed to check if GradleFile exist at: %s, error: %s", configs.GradleFile, err)
		} else if !exist {
			return "", fmt.Errorf("GradleFile not exist at: %s", configs.GradleFile)
		}
	}

	if configs.UnitTestTasks == "" {
		return "", errors.New("no UnitTestTasks parameter specified")
	}

	if configs.GradlewPath == "" {
		explanation := `
Using a Gradle Wrapper (gradlew) is required, as the wrapper is what makes sure
that the right Gradle version is installed and used for the build.

You can find more information about the Gradle Wrapper (gradlew),
and about how you can generate one (if you would not have one already
in the official guide at: https://docs.gradle.org/current/userguide/gradle_wrapper.html`

		return explanation, errors.New("no GradlewPath parameter specified")
	}
	if exist, err := pathutil.IsPathExists(configs.GradlewPath); err != nil {
		return "", fmt.Errorf("Failed to check if GradlewPath exist at: %s, error: %s", configs.GradlewPath, err)
	} else if !exist {
		return "", fmt.Errorf("GradlewPath not exist at: %s", configs.GradlewPath)
	}

	if err := input.ValidateIfNotEmpty(configs.CacheLevel); err != nil {
		return "", fmt.Errorf("CacheLevel, error: %s", err)
	}

	if err := input.ValidateWithOptions(configs.CacheLevel, "all", "only deps", "none"); err != nil {
		return "", fmt.Errorf("CacheLevel, error: %s", err)
	}

	return "", nil
}

func runGradleTask(gradleTool, buildFile, tasks, options string) error {
	optionSlice, err := shellquote.Split(options)
	if err != nil {
		return err
	}

	taskSlice, err := shellquote.Split(tasks)
	if err != nil {
		return err
	}

	cmdSlice := []string{gradleTool}
	if buildFile != "" {
		cmdSlice = append(cmdSlice, "--build-file", buildFile)
	}
	cmdSlice = append(cmdSlice, taskSlice...)
	cmdSlice = append(cmdSlice, optionSlice...)

	log.Donef("$ %s", command.PrintableCommandArgs(false, cmdSlice))
	fmt.Println()

	cmd, err := command.NewFromSlice(cmdSlice)
	if err != nil {
		return fmt.Errorf("failed to create command, error: %s", err)
	}

	cmd.SetStdout(os.Stdout)
	cmd.SetStderr(os.Stderr)

	return cmd.Run()
}

func exportEnvironmentWithEnvman(keyStr, valueStr string) error {
	cmd := command.New("envman", "add", "--key", keyStr)
	cmd.SetStdin(strings.NewReader(valueStr))
	return cmd.Run()
}

func computeMD5String(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Errorf("Failed to close file(%s), error: %s", filePath, err)
		}
	}()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func main() {
	configs := createConfigsModelFromEnvs()

	fmt.Println()
	configs.print()

	if explanation, err := configs.validate(); err != nil {
		fmt.Println()
		log.Errorf("Issue with input: %s", err)
		fmt.Println()

		if explanation != "" {
			fmt.Println(explanation)
			fmt.Println()
		}

		os.Exit(1)
	}

	err := os.Chmod(configs.GradlewPath, 0770)
	if err != nil {
		log.Errorf("Failed to add executable permission on gradlew file (%s), error: %s", configs.GradlewPath, err)
		os.Exit(1)
	}

	fmt.Println()
	log.Infof("Running gradle task...")
	if err := runGradleTask(configs.GradlewPath, configs.GradleFile, configs.UnitTestTasks, configs.UnitTestFlags); err != nil {
		log.Errorf("Gradle task failed, error: %s", err)

		if err := exportEnvironmentWithEnvman("BITRISE_GRADLE_TEST_RESULT", "failed"); err != nil {
			log.Warnf("Failed to export environment: %s, error: %s", "BITRISE_GRADLE_TEST_RESULT", err)
		}

		os.Exit(1)
	}

	// Collecting caches
	if configs.CacheLevel != "none" {
		fmt.Println()
		log.Infof("Collecting gradle caches...")

		gradleCache := cache.New()
		homeDir := pathutil.UserHomeDir()
		collectCaches := true
		includePths := []string{}

		projectRoot, err := filepath.Abs(".")
		if err != nil {
			log.Warnf("Cache collection skipped: failed to determine project root path.")
			collectCaches = false
		}

		lockFilePath := filepath.Join(projectRoot, "gradle.deps")

		if configs.CacheLevel == "all" || configs.CacheLevel == "only deps" {

			// create dependencies lockfile
			log.Printf(" Generate dependencies map...")
			lockfileContent := ""
			if err := filepath.Walk(projectRoot, func(path string, f os.FileInfo, err error) error {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".gradle") && !strings.Contains(path, "node_modules") {
					if md5Hash, err := computeMD5String(path); err != nil {
						log.Warnf("Failed to compute MD5 hash of file(%s), error: %s", path, err)
					} else {
						lockfileContent += md5Hash
					}
				}
				return nil
			}); err != nil {
				log.Warnf("Dependency map generation skipped: failed to collect dependencies.")
				collectCaches = false
			} else {
				err := fileutil.WriteStringToFile(lockFilePath, lockfileContent)
				if err != nil {
					log.Warnf("Dependency map generation skipped: failed to write lockfile, error: %s", err)
					collectCaches = false
				}
			}

			includePths = append(includePths, fmt.Sprintf("%s -> %s", filepath.Join(homeDir, ".gradle"), lockFilePath))
			includePths = append(includePths, fmt.Sprintf("%s -> %s", filepath.Join(homeDir, ".kotlin"), lockFilePath))
			includePths = append(includePths, fmt.Sprintf("%s -> %s", filepath.Join(homeDir, ".m2"), lockFilePath))
		}

		if configs.CacheLevel == "all" {
			includePths = append(includePths, fmt.Sprintf("%s -> %s", filepath.Join(homeDir, ".android", "build-cache"), lockFilePath))
		}

		excludePths := []string{
			"~/.gradle/**",
			"~/.android/build-cache/**",
			"*.lock",
			"*.bin",
			"/**/build/**.json",
			"/**/build/**.html",
			"/**/build/**.xml",
			"/**/build/**.properties",
			"/**/build/**/zip-cache/**",
			"*.log",
			"*.txt",
			"*.rawproto",
			"!*.ap_",
			"!*.apk",
		}

		if configs.CacheLevel == "all" {
			if err := filepath.Walk(projectRoot, func(path string, f os.FileInfo, err error) error {
				if f.IsDir() {
					if f.Name() == "build" {
						includePths = append(includePths, path)
					}
					if f.Name() == ".gradle" {
						includePths = append(includePths, path)
					}
				}
				return nil
			}); err != nil {
				log.Warnf("Cache collection skipped: failed to determine cache paths.")
				collectCaches = false
			}
		}
		if collectCaches {
			gradleCache.IncludePath(strings.Join(includePths, "\n"))
			gradleCache.ExcludePath(strings.Join(excludePths, "\n"))

			if err := gradleCache.Commit(); err != nil {
				log.Warnf("Cache collection skipped: failed to commit cache paths.")
			}
		}
		log.Donef("Done")
	}

	if err := exportEnvironmentWithEnvman("BITRISE_GRADLE_TEST_RESULT", "succeeded"); err != nil {
		log.Warnf("Failed to export environment: %s, error: %s", "BITRISE_GRADLE_TEST_RESULT", err)
	}
}
