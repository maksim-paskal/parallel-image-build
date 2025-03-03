package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maksim-paskal/parallel-image-build/types"
	"github.com/pkg/errors"
)

var Version = "dev" //nolint:gochecknoglobals

func NewApplication() *Application {
	return &Application{
		ImageMetadata: types.NewImageMetadata(),
	}
}

type Application struct {
	Provider             types.FlagProvider
	ProviderArgs         types.FlagProviderArgs
	Platform             types.FlagPlatform
	Registry             types.FlagRegistry
	ImageContext         types.FlagList
	ImagePath            types.FlagList
	ImageDockerfile      types.FlagList
	ImageArgs            types.FlagList
	Tag                  types.FlagList
	GitlabBranchPlatform types.FlagString
	GitlabBranchRegistry types.FlagString
	WithAttestation      bool
	ImageMetadata        types.ImageMetadata

	CheckImageAnnotation    bool
	CheckImageAnnotationKey string
}

func (a *Application) shell(ctx context.Context, name string, arg ...string) error {
	slog.Debug("Running command", "name", name, "args", arg)

	cmd := exec.CommandContext(ctx, name, arg...)

	logger := types.ShellLogger{}

	if group, ok := ctx.Value(types.ContextKeyGroup).(string); ok {
		logger.Group = group
	}

	cmd.Stdout = &logger
	cmd.Stderr = &logger

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "failed to run command")
	}

	return nil
}

type DockerImageInspect struct {
	Annotations map[string]string `json:"annotations"`
}

// inspecting images, return empty map.
func (a *Application) inspectDockerTag(ctx context.Context, tag string) DockerImageInspect {
	slog.Debug("Inspecting docker tag", "tag", tag)

	buffer := bytes.Buffer{}

	cmd := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", tag)
	cmd.Stdout = &buffer

	result := DockerImageInspect{}

	// if command failed return empty result
	if err := cmd.Run(); err != nil {
		slog.Warn("Failed to inspect docker tag", "tag", tag, "error", err.Error())

		return result
	}

	if err := json.NewDecoder(&buffer).Decode(&result); err != nil {
		slog.Error("Failed to decode docker tag", "tag", tag, "error", err.Error())

		return result
	}

	return result
}

// return true if local and remote image annotations are equal.
func (a *Application) ignoreBuild(ctx context.Context, tag string) bool {
	if !a.CheckImageAnnotation {
		return false
	}

	// get current build annotations
	current := a.ImageMetadata.GetBuildMetadata()[a.CheckImageAnnotationKey]

	if current == "" {
		return false
	}

	// get remote build annotations
	remote := a.inspectDockerTag(ctx, tag).Annotations[a.CheckImageAnnotationKey]

	slog.Info("Checking image annotation",
		"tag", tag,
		"key", a.CheckImageAnnotationKey,
		"current", current,
		"remote", remote,
	)

	return current == remote && remote != ""
}

func (a *Application) buildImageArch(ctx context.Context, i int, platform types.Platform) error {
	image := a.ImagePath[i] + "-" + platform.Arch

	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/"+platform.Arch)

	slog.Info("Start building...", "image", image, "index", i)

	args := []string{
		"--platform=" + platform.String(),
		"--file=" + a.ImageDockerfile[i],
		a.ImageContext[i],
	}

	if len(a.ImageArgs[i]) > 0 {
		args = append(args, a.ImageArgs[i])
	}

	// add build annotations
	args = append(args, a.ImageMetadata.GetBuildAnnotations()...)

	tags := []string{}

	for _, registry := range a.Registry {
		tag := registry + image

		if a.ignoreBuild(ctx, tag) {
			slog.Info("Skipping build", "tag", tag)

			continue
		}

		tags = append(tags, "--tag="+registry+image)
	}

	// do not build image if no tags found
	if len(tags) == 0 {
		return nil
	}

	// append tags to build args
	args = append(args, tags...)

	buildArgs := append(a.Provider.ProgramArgs(a.WithAttestation), a.ProviderArgs...)
	buildArgs = append(buildArgs, args...)

	if err := a.shell(ctx, a.Provider.Program(), buildArgs...); err != nil {
		return errors.Wrap(err, "failed to build image")
	}

	return nil
}

func (a *Application) publishManifestRegistry(ctx context.Context, registry string, i int) error {
	image := registry + a.ImagePath[i]

	manifestCreateArgs := []string{
		"buildx",
		"imagetools",
		"create",
		"-t",
		image,
	}

	for _, platform := range a.Platform {
		manifestCreateArgs = append(manifestCreateArgs, image+"-"+platform.Arch)
	}

	slog.Info("Start publishing manifest...", "image", image)
	// create
	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/manifest/create")

	if err := a.shell(ctx, a.Provider.Program(), manifestCreateArgs...); err != nil {
		return errors.Wrap(err, "failed to create manifest")
	}

	manifestInspectArgs := []string{
		"buildx",
		"imagetools",
		"inspect",
		image,
	}

	// inspect
	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/manifest/inspect")

	if err := a.shell(ctx, a.Provider.Program(), manifestInspectArgs...); err != nil {
		return errors.Wrap(err, "failed to create manifest")
	}

	return nil
}

func (a *Application) publishManifest(ctx context.Context, i int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg := sync.WaitGroup{}

	for _, registry := range a.Registry {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := a.publishManifestRegistry(ctx, registry, i); err != nil {
				slog.Error("failed to publish manifest", "error", err.Error())
				cancel()
			}
		}()
	}

	wg.Wait()

	return ctx.Err() //nolint:wrapcheck
}

func (a *Application) buildImage(ctx context.Context, i int) error {
	startTime := time.Now()

	defer func() {
		slog.Info("Finished", "image", a.ImagePath[i], "duration", time.Since(startTime).Round(time.Second))
	}()

	wg := sync.WaitGroup{}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i))

	for _, platform := range a.Platform {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := a.buildImageArch(ctx, i, platform); err != nil {
				slog.Error("failed to build image", "error", err.Error())
				cancel()
			}
		}()
	}

	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck
	}

	if err := a.publishManifest(ctx, i); err != nil {
		return errors.Wrap(err, "failed to publish manifest")
	}

	return nil
}

func (a *Application) loadFromTags() {
	if len(a.Tag) == 0 {
		return
	}

	imagePath := make(types.FlagList, len(a.Tag))

	for i, tag := range a.Tag {
		imagePath[i] = a.ImagePath.Get(i, tag)
	}

	a.ImagePath = imagePath
}

// return true if gitlab pipeline is running on branch.
func (a *Application) isGitlabPipelineRunOnBranch() bool {
	if tag := os.Getenv("CI_COMMIT_TAG"); len(tag) > 0 {
		return false
	}

	return true
}

func (a *Application) loadFromEnv() {
	fromEnv := func(key string, value interface{}) {
		env := os.Getenv(key)

		if len(env) == 0 {
			return
		}

		switch v := value.(type) {
		case *bool:
			if b, err := strconv.ParseBool(env); err == nil {
				*v = b
			}
		case *string:
			*v = env
		}
	}

	fromEnv("PARALLEL_IMAGE_BUILD_CHECK_IMAGE_ANNOTATION", &a.CheckImageAnnotation)
	fromEnv("PARALLEL_IMAGE_BUILD_CHECK_IMAGE_ANNOTATION_KEY", &a.CheckImageAnnotationKey)
}

func (a *Application) Normalize() error { //nolint:cyclop
	a.loadFromEnv()

	if len(a.Provider) == 0 {
		if err := a.Provider.Set(string(types.FlagProviderBuildx)); err != nil {
			return errors.Wrap(err, "failed to set provider")
		}
	}

	if platform := os.Getenv("PARALLEL_IMAGE_BUILD_PLATFORM"); len(platform) > 0 {
		if err := a.Platform.Set(platform); err != nil {
			return errors.Wrap(err, "failed to set platform from env")
		}
	}

	if len(a.Platform) == 0 {
		if err := a.Platform.Set("linux/amd64,linux/arm64"); err != nil {
			return errors.Wrap(err, "failed to set platform")
		}
	}

	a.loadFromTags()

	if len(a.Registry) == 0 {
		if err := a.Registry.Set("docker.io"); err != nil {
			return errors.Wrap(err, "failed to set registry")
		}
	}

	imageContext := make(types.FlagList, len(a.ImagePath))
	imageDockerfile := make(types.FlagList, len(a.ImagePath))
	imageArgs := make(types.FlagList, len(a.ImagePath))

	for i := range a.ImagePath {
		imageContext[i] = a.ImageContext.Get(i, ".")
		imageDockerfile[i] = a.ImageDockerfile.Get(i, imageContext[i]+"/Dockerfile")
		imageArgs[i] = a.ImageArgs.Get(i, "")
	}

	a.ImageContext = imageContext
	a.ImageDockerfile = imageDockerfile
	a.ImageArgs = imageArgs

	// check gitlab pipeline platform
	if len(a.GitlabBranchPlatform) > 0 && a.isGitlabPipelineRunOnBranch() {
		a.Platform = types.FlagPlatform{}

		if err := a.Platform.Set(a.GitlabBranchPlatform.String()); err != nil {
			return errors.Wrap(err, "failed to set platform from gitlab")
		}
	}

	// check gitlab pipeline registry
	if len(a.GitlabBranchRegistry) > 0 && a.isGitlabPipelineRunOnBranch() {
		a.Registry = types.FlagRegistry{}

		if err := a.Registry.Set(a.GitlabBranchRegistry.String()); err != nil {
			return errors.Wrap(err, "failed to set registry from gitlab")
		}
	}

	return nil
}

func (a *Application) Validate() error {
	if err := a.Normalize(); err != nil {
		return errors.Wrap(err, "failed to normalize")
	}

	if len(a.Provider) == 0 {
		return errors.New("provider is empty")
	}

	if len(a.Registry) == 0 {
		return errors.New("registry is empty")
	}

	if len(a.ImagePath) == 0 {
		return errors.New("image-path is empty")
	}

	if len(a.ImageContext) != len(a.ImagePath) {
		return errors.New("image-context is invalid")
	}

	if len(a.ImageDockerfile) != len(a.ImagePath) {
		return errors.New("image-dockerfile is invalid")
	}

	for i := range a.ImagePath {
		if !strings.HasPrefix(a.ImagePath[i], "/") {
			a.ImagePath[i] = "/" + a.ImagePath[i]
		}
	}

	return nil
}

func (a *Application) Run(ctx context.Context) error {
	slog.Info("Application is running", "instance", a)
	slog.Debug("Images", "len", len(a.ImagePath))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg := sync.WaitGroup{}

	for i := range a.ImagePath {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := a.buildImage(ctx, i); err != nil {
				slog.Error("failed to build image", "error", err.Error())
				cancel()
			}
		}()
	}

	wg.Wait()

	return ctx.Err() //nolint:wrapcheck
}
