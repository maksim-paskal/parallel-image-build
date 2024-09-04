package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maksim-paskal/parallel-image-build/internal"
)

func main() { //nolint:funlen
	application := internal.NewApplication()

	flag.Var(&application.Provider, "provider", "what provider to use")
	flag.Var(&application.ProviderArgs, "provider-args", "arguments for provider")
	flag.Var(&application.Platform, "platform", "platforms to use")
	flag.Var(&application.Registry, "registry", "registry to push image")
	flag.Var(&application.ImagePath, "image-path", "path to image")
	flag.Var(&application.ImageContext, "image-context", "path to image context")
	flag.Var(&application.ImageDockerfile, "image-dockerfile", "path to image dockerfile")

	flag.Var(&application.Tag, "tag", "tag to use")

	debug := flag.Bool("debug", false, "debug mode")
	gitlabBranchRegistry := flag.String("gitlab-branch-registry", "", "platform to use when no tag is found in gitlab")
	gitlabBranchPlatform := flag.String("gitlab-branch-platform", "", "platform to use when no tag is found in gitlab")
	withAttestation := flag.Bool("with-attestation", true, "publish attestation on build")

	flag.Parse()

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	application.GitlabBranchPlatform = *gitlabBranchPlatform
	application.GitlabBranchRegistry = *gitlabBranchRegistry
	application.WithAttestation = *withAttestation

	if err := application.Validate(); err != nil {
		slog.Error("Error validating", "error", err.Error())

		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalChanInterrupt := make(chan os.Signal, 1)
	signal.Notify(signalChanInterrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-signalChanInterrupt
		slog.Warn("Received interrupt signal")
		cancel()
		<-signalChanInterrupt
		os.Exit(1)
	}()

	startTime := time.Now()

	if err := application.Run(ctx); err != nil {
		slog.Error("failed to run application", "error", err.Error())
		cancel()

		slog.Warn("Cancel context...")
		time.Sleep(5 * time.Second) //nolint:mnd
		os.Exit(1)                  //nolint:gocritic
	}

	slog.Info("Finished",
		"duration", time.Since(startTime).Round(time.Second),
		"images", len(application.ImagePath),
		"platforms", len(application.Platform),
	)
}
