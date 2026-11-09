package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maksim-paskal/parallel-image-build/types"
	"github.com/pkg/errors"
)

const (
	buildxCommand = "buildx"

	// returned by buildx when the selected builder driver cannot produce attestations.
	attestationNotSupportedError = "Attestation is not supported"
)

var Version = "dev" //nolint:gochecknoglobals

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

	Compression      string
	CompressionLevel int
	CompressionForce bool

	// https://docs.docker.com/build/exporters/image-registry
	Output string

	BuildArgs    types.FlagList
	BuildSecrets types.FlagList
}

func NewApplication() *Application {
	return &Application{
		Provider:             "",
		ProviderArgs:         nil,
		Platform:             nil,
		Registry:             nil,
		ImageContext:         nil,
		ImagePath:            nil,
		ImageDockerfile:      nil,
		ImageArgs:            nil,
		Tag:                  nil,
		GitlabBranchPlatform: "",
		GitlabBranchRegistry: "",
		ImageMetadata:        types.NewImageMetadata(),
		Output:               "",
		BuildArgs:            nil,
		BuildSecrets:         nil,

		// Publish SBOM and provenance attestations, requires registry > v3.0.0+
		// and a builder that supports attestations (falls back to no attestation otherwise).
		WithAttestation: true,

		CheckImageAnnotation:    true,
		CheckImageAnnotationKey: "org.opencontainers.image.revision",

		Compression:      "zstd",
		CompressionLevel: 3, //nolint:mnd
		CompressionForce: true,
	}
}

func (a *Application) Normalize() error { //nolint:cyclop,funlen
	a.loadFromEnv()

	if len(a.Provider) == 0 {
		err := a.Provider.Set(string(types.FlagProviderBuildx))
		if err != nil {
			return errors.Wrap(err, "failed to set provider")
		}
	}

	if platform := os.Getenv("PARALLEL_IMAGE_BUILD_PLATFORM"); len(platform) > 0 {
		a.Platform = types.FlagPlatform{}

		err := a.Platform.Set(platform)
		if err != nil {
			return errors.Wrap(err, "failed to set platform from env")
		}
	}

	if len(a.Platform) == 0 {
		err := a.Platform.Set("linux/amd64,linux/arm64")
		if err != nil {
			return errors.Wrap(err, "failed to set platform")
		}
	}

	a.loadFromTags()

	if len(a.Registry) == 0 {
		err := a.Registry.Set("docker.io")
		if err != nil {
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

		err := a.Platform.Set(a.GitlabBranchPlatform.String())
		if err != nil {
			return errors.Wrap(err, "failed to set platform from gitlab")
		}
	}

	// check gitlab pipeline registry
	if len(a.GitlabBranchRegistry) > 0 && a.isGitlabPipelineRunOnBranch() {
		a.Registry = types.FlagRegistry{}

		err := a.Registry.Set(a.GitlabBranchRegistry.String())
		if err != nil {
			return errors.Wrap(err, "failed to set registry from gitlab")
		}
	}

	a.Output = a.mergeOutput()

	return nil
}

func (a *Application) Validate() error {
	err := a.Normalize()
	if err != nil {
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
		wg.Go(func() {
			err := a.buildImage(ctx, i)
			if err != nil {
				slog.Error("failed to build image", "error", err.Error())
				cancel()
			}
		})
	}

	wg.Wait()

	return ctx.Err() //nolint:wrapcheck
}

func (a *Application) mergeOutput() string {
	output := make(map[string]string)

	// default buildx output
	output["type"] = "image"
	output["oci-mediatypes"] = "true"

	// compression
	output["compression"] = a.Compression
	if a.CompressionLevel > -1 {
		output["compression-level"] = strconv.Itoa(a.CompressionLevel)
	}

	output["force-compression"] = strconv.FormatBool(a.CompressionForce)

	const parts = 2

	// append user output parameters
	for param := range strings.SplitSeq(a.Output, ",") {
		if len(param) == 0 {
			continue
		}

		a := strings.SplitN(param, "=", parts)

		if len(a) != parts {
			slog.Warn("Invalid output parameter", "param", param)

			continue
		}

		output[a[0]] = a[1]
	}

	keys := slices.Sorted(maps.Keys(output))
	result := make([]string, 0, len(keys))

	for _, key := range keys {
		result = append(result, key+"="+output[key])
	}

	return strings.Join(result, ",")
}

func (a *Application) shell(ctx context.Context, name string, arg ...string) error {
	_, err := a.shellOutput(ctx, name, arg...)

	return err
}

// run command, log its output and return it.
func (a *Application) shellOutput(ctx context.Context, name string, arg ...string) (string, error) {
	slog.Debug("Running command", "name", name, "args", arg)

	cmd := exec.CommandContext(ctx, name, arg...)

	logger := types.ShellLogger{Group: ""}

	if group, ok := ctx.Value(types.ContextKeyGroup).(string); ok {
		logger.Group = group
	}

	output := bytes.Buffer{}
	writer := io.MultiWriter(&logger, &output)

	// same writer for stdout and stderr, so writes are not concurrent
	cmd.Stdout = writer
	cmd.Stderr = writer

	err := cmd.Run()
	if err != nil {
		return output.String(), errors.Wrap(err, "failed to run command")
	}

	return output.String(), nil
}

type DockerImageInspect struct {
	Annotations map[string]string     `json:"annotations"`
	Manifests   []DockerImageManifest `json:"manifests"`
}

type DockerImageManifest struct {
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
}

// returns tag without tag suffix, registry:5000/image:tag -> registry:5000/image.
func imageName(tag string) string {
	if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
		return tag[:i]
	}

	return tag
}

// inspecting images, return empty map.
func (a *Application) inspectDockerTag(ctx context.Context, tag string) DockerImageInspect {
	slog.Debug("Inspecting docker tag", "tag", tag)

	buffer := bytes.Buffer{}

	name := "docker"
	args := []string{buildxCommand, "imagetools", "inspect", "--raw", tag}

	slog.Debug("Running command", "name", name, "args", args)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &buffer

	result := DockerImageInspect{Annotations: nil, Manifests: nil}

	// if command failed return empty result
	err := cmd.Run()
	if err != nil {
		slog.Warn("Failed to inspect docker tag", "tag", tag, "error", err.Error())

		return result
	}

	err = json.NewDecoder(&buffer).Decode(&result)
	if err != nil {
		slog.Error("Failed to decode docker tag", "tag", tag, "error", err.Error())

		return result
	}

	// with attestation the tag is an index, build annotations are set on the image manifest inside it
	if len(result.Annotations) == 0 {
		for _, manifest := range result.Manifests {
			// skip attestation manifests
			if manifest.Annotations["vnd.docker.reference.type"] != "" {
				continue
			}

			return a.inspectDockerTag(ctx, imageName(tag)+"@"+manifest.Digest)
		}
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

func (a *Application) buildArgs() []string {
	args := make([]string, 0, len(a.BuildArgs)+len(a.BuildSecrets))

	for _, buildArg := range a.BuildArgs {
		args = append(args, "--build-arg="+buildArg)
	}

	for _, buildSecret := range a.BuildSecrets {
		args = append(args, "--secret="+buildSecret)
	}

	return args
}

func (a *Application) buildImageArch(ctx context.Context, i int, platform types.Platform) error {
	image := a.ImagePath[i] + "-" + platform.Arch

	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/"+platform.Arch)

	slog.Info("Start building...", "image", image, "index", i)

	args := []string{
		"--platform=" + platform.String(),
		"--file=" + a.ImageDockerfile[i],
		"--output=" + a.Output,
		a.ImageContext[i],
	}

	if len(a.ImageArgs[i]) > 0 {
		args = append(args, a.ImageArgs[i])
	}

	// add build args
	args = append(args, a.buildArgs()...)

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

	output, err := a.shellOutput(ctx, a.Provider.Program(), buildArgs...)
	if err != nil && a.WithAttestation && strings.Contains(output, attestationNotSupportedError) {
		slog.Warn("Builder does not support attestation, retrying without it", "image", image)

		buildArgs = append(a.Provider.ProgramArgs(false), a.ProviderArgs...)
		buildArgs = append(buildArgs, args...)

		err = a.shell(ctx, a.Provider.Program(), buildArgs...)
	}

	if err != nil {
		return errors.Wrap(err, "failed to build image")
	}

	return nil
}

func (a *Application) publishManifestRegistry(ctx context.Context, registry string, i int) error {
	image := registry + a.ImagePath[i]

	manifestCreateArgs := append(make([]string, 0, 5+len(a.Platform)), buildxCommand, "imagetools", "create", "-t", image)

	for _, platform := range a.Platform {
		manifestCreateArgs = append(manifestCreateArgs, image+"-"+platform.Arch)
	}

	slog.Info("Start publishing manifest...", "image", image)
	// create
	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/manifest/create")

	err := a.shell(ctx, a.Provider.Program(), manifestCreateArgs...)
	if err != nil {
		return errors.Wrap(err, "failed to create manifest")
	}

	manifestInspectArgs := []string{
		buildxCommand,
		"imagetools",
		"inspect",
		image,
	}

	// inspect
	ctx = context.WithValue(ctx, types.ContextKeyGroup, strconv.Itoa(i)+"/manifest/inspect")

	err = a.shell(ctx, a.Provider.Program(), manifestInspectArgs...)
	if err != nil {
		return errors.Wrap(err, "failed to create manifest")
	}

	return nil
}

func (a *Application) publishManifest(ctx context.Context, i int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg := sync.WaitGroup{}

	for _, registry := range a.Registry {
		wg.Go(func() {
			err := a.publishManifestRegistry(ctx, registry, i)
			if err != nil {
				slog.Error("failed to publish manifest", "error", err.Error())
				cancel()
			}
		})
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
		wg.Go(func() {
			err := a.buildImageArch(ctx, i, platform)
			if err != nil {
				slog.Error("failed to build image", "error", err.Error())
				cancel()
			}
		})
	}

	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck
	}

	err := a.publishManifest(ctx, i)
	if err != nil {
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
	fromEnv := func(key string, value any) {
		env := os.Getenv(key)

		if len(env) == 0 {
			return
		}

		switch v := value.(type) {
		case *bool:
			b, err := strconv.ParseBool(env)
			if err == nil {
				*v = b
			}
		case *string:
			*v = env
		}
	}

	fromEnv("PARALLEL_IMAGE_BUILD_CHECK_IMAGE_ANNOTATION", &a.CheckImageAnnotation)
	fromEnv("PARALLEL_IMAGE_BUILD_CHECK_IMAGE_ANNOTATION_KEY", &a.CheckImageAnnotationKey)
	fromEnv("PARALLEL_IMAGE_BUILD_WITH_ATTESTATION", &a.WithAttestation)
}
