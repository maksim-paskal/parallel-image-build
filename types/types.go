package types

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

type FlagString string

func (p *FlagString) Set(value string) error {
	*p = FlagString(value)

	return nil
}

func (p *FlagString) String() string {
	return string(*p)
}

type FlagList []string

func (p *FlagList) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *FlagList) Get(i int, defaultValue string) string {
	if len(*p) > i {
		return (*p)[i]
	}

	return defaultValue
}

func (p *FlagList) Set(value string) error {
	*p = append(*p, value)

	return nil
}

type Platform struct {
	OS   string
	Arch string
}

func (p *Platform) String() string {
	return fmt.Sprintf("%s/%s", p.OS, p.Arch)
}

type FlagPlatform []Platform

func (p *FlagPlatform) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *FlagPlatform) Set(value string) error {
	for _, platform := range strings.Split(value, ",") {
		arch := strings.Split(platform, "/")

		if len(arch) != 2 { //nolint:mnd
			return errors.Errorf("invalid platform: %s", platform)
		}

		*p = append(*p, struct {
			OS   string
			Arch string
		}{
			OS:   arch[0],
			Arch: arch[1],
		})
	}

	return nil
}

type FlagProvider string

var FlagProviderValid = []FlagProvider{FlagProviderBuildx} //nolint:gochecknoglobals

const FlagProviderBuildx FlagProvider = "buildx"

func (p *FlagProvider) ProgramArgs(attestation bool) []string {
	if *p == FlagProviderBuildx {
		return []string{
			"buildx",
			"build",
			"--pull",
			"--push",
			"--sbom=" + strconv.FormatBool(attestation),
			"--provenance=" + strconv.FormatBool(attestation),
		}
	}

	return []string{}
}

func (p *FlagProvider) Program() string {
	if *p == FlagProviderBuildx {
		return "docker"
	}

	return ""
}

func (p *FlagProvider) String() string {
	return string(*p)
}

func (p *FlagProvider) Set(value string) error {
	if !slices.Contains(FlagProviderValid, FlagProvider(value)) {
		return errors.Errorf("invalid use: %s, valid %+v", value, FlagProviderValid)
	}

	*p = FlagProvider(value)

	return nil
}

type FlagRegistry []string

func (p *FlagRegistry) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *FlagRegistry) Set(value string) error {
	for _, registry := range strings.Split(value, ",") {
		*p = append(*p, registry)
	}

	return nil
}

type FlagProviderArgs []string

func (p *FlagProviderArgs) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *FlagProviderArgs) Set(value string) error {
	for _, registry := range strings.Split(value, " ") {
		*p = append(*p, registry)
	}

	return nil
}

type ContextKey string

const ContextKeyGroup ContextKey = "group"

type ShellLogger struct {
	Group string
}

func (s *ShellLogger) Write(p []byte) (int, error) {
	prefix := "[" + s.Group + "]"
	out := strings.ReplaceAll(string(p), "\n", "\n"+prefix)

	fmt.Println(prefix + out) //nolint:forbidigo

	return len(p), nil
}

func NewImageMetadata() ImageMetadata {
	m := ImageMetadata{
		Created: time.Now().Format(time.RFC3339),
	}

	return m
}

// https://github.com/opencontainers/image-spec/blob/main/annotations.md#pre-defined-annotation-keys
type ImageMetadata struct {
	// Date and time on which the image was built, conforming to RFC 3339
	Created string
	// Human-readable title of the image (string)
	Title string
	// Source control revision identifier for the packaged software.
	Revision string
	// Version of the packaged software
	Version string
}

// returns build annotations arguments.
func (m *ImageMetadata) GetBuildAnnotations() []string {
	result := []string{}

	for k, v := range m.GetBuildMetadata() {
		if v == "" {
			continue
		}

		result = append(result, fmt.Sprintf("--annotation=%s=%s", k, v))
	}

	return result
}

// returns map of build metadata.
func (m *ImageMetadata) GetBuildMetadata() map[string]string {
	result := map[string]string{}

	annotation := func(name, value string) {
		if value == "" {
			return
		}

		result[name] = value
	}

	annotation("org.opencontainers.image.created", m.Created)
	annotation("org.opencontainers.image.title", m.Title)
	annotation("org.opencontainers.image.revision", m.Revision)
	annotation("org.opencontainers.image.version", m.Version)

	// add Gitlab CI metadata
	annotation("com.gitlab.ci.user.name", os.Getenv("GITLAB_USER_NAME"))
	annotation("com.gitlab.ci.pipeline.id", os.Getenv("CI_PIPELINE_ID"))
	annotation("com.gitlab.ci.pipeline.url", os.Getenv("CI_PIPELINE_URL"))
	annotation("com.gitlab.ci.job.id", os.Getenv("CI_JOB_ID"))
	annotation("com.gitlab.ci.job.url", os.Getenv("CI_JOB_URL"))
	annotation("com.gitlab.ci.commit.sha", os.Getenv("CI_COMMIT_SHA"))
	annotation("com.gitlab.ci.commit.ref.name", os.Getenv("CI_COMMIT_REF_NAME"))
	annotation("com.gitlab.ci.project.path", os.Getenv("CI_PROJECT_PATH"))
	annotation("org.opencontainers.image.source", os.Getenv("CI_PROJECT_URL"))
	annotation("org.opencontainers.image.revision", os.Getenv("CI_COMMIT_SHA"))

	return result
}
