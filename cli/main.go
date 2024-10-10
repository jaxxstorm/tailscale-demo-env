package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"encoding/json"
	"github.com/alecthomas/kingpin/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Project struct {
	Name          string
	Path          string
	AllowedStacks []string
}

type ProjectSource struct {
	IsGit     bool
	GitURL    string
	GitBranch string
	LocalPath string
}

var (
	app        = kingpin.New("demo-env-deployer", "A command-line application deployment tool using pulumi.")
	deployCmd  = app.Command("deploy", "Deploy the demo env.")
	destroyCmd = app.Command("destroy", "Destroy the demo env.")

	gitRepoURL  = app.Flag("git-url", "URL of the Git repository").String()
	gitBranch   = app.Flag("git-branch", "Git branch to use").Default("main").String()
	localPath   = app.Flag("path", "Path to local directory containing projects").String()
	jsonLogging = app.Flag("json", "Enable JSON logging").Bool()
	stacks      = app.Flag("stacks", "Stacks to deploy").Default("west", "east", "eu").Strings()
)

func createOrSelectStack(ctx context.Context, stackName string, project Project, source ProjectSource) (auto.Stack, error) {
	if source.IsGit {
		repo := auto.GitRepo{
			URL:         source.GitURL,
			Branch:      source.GitBranch,
			ProjectPath: project.Path,
		}
		return auto.UpsertStackRemoteSource(ctx, stackName, repo)
	}
	projectPath := filepath.Join(source.LocalPath, project.Path)
	return auto.UpsertStackLocalSource(ctx, stackName, projectPath)
}

func createOutputLogger() *zap.Logger {
	encoderConfig := zap.NewDevelopmentEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)

	core := zapcore.NewCore(consoleEncoder, zapcore.Lock(os.Stdout), zapcore.DebugLevel)

	sampling := zapcore.NewSamplerWithOptions(
		core,
		time.Second,
		3,
		0,
	)

	return zap.New(sampling)
}

func processEvents(logger *zap.Logger, eventChannel <-chan events.EngineEvent) {
	for event := range eventChannel {
		jsonData, err := json.Marshal(event)
		if err != nil {
			logger.Error("Failed to marshal event to JSON", zap.Error(err))
			continue
		}
		logger.Info(string(jsonData))
	}
}

func deploy(stack string, projects []Project, source ProjectSource) {
	logger := createOutputLogger().With(zap.String("stack", stack), zap.Any("projects", projects))
	defer logger.Sync()

	logger.Info("Starting deployment")

	ctx := context.Background()

	deployProject := func(project Project) error {
		if len(project.AllowedStacks) > 0 && !contains(project.AllowedStacks, stack) {
			logger.Info("Skipping project (not in allowed stacks)", zap.String("project", project.Name))
			return nil
		}

		logger.Info("Deploying project", zap.String("project", project.Name))
		eventChannel := make(chan events.EngineEvent)
		go processEvents(logger, eventChannel)

		s, err := createOrSelectStack(ctx, stack, project, source)
		if err != nil {
			logger.Error("Failed to create or select stack", zap.Error(err))
			return err
		}

		var upErr error
		if *jsonLogging {
			_, upErr = s.Up(ctx, optup.EventStreams(eventChannel))
		} else {
			_, upErr = s.Up(ctx, optup.ProgressStreams(os.Stdout))
		}
		if upErr != nil {
			logger.Error("Failed to update project", zap.String("project", project.Name), zap.Error(upErr))
		} else {
			logger.Info("Successfully deployed project", zap.String("project", project.Name))
		}
		return upErr
	}

	for _, project := range projects {
		if err := deployProject(project); err != nil {
			logger.Error("Failed to deploy project", zap.String("project", project.Name), zap.Error(err))
			return
		}
	}

	logger.Info("Completed deployment")
}

func destroy(stack string, projects []Project, source ProjectSource) {
	logger := createOutputLogger().With(zap.String("stack", stack))
	defer logger.Sync()

	logger.Info("Starting destruction")

	ctx := context.Background()

	destroyProject := func(project Project) error {
		if len(project.AllowedStacks) > 0 && !contains(project.AllowedStacks, stack) {
			logger.Info("Skipping project (not in allowed stacks)", zap.String("project", project.Name))
			return nil
		}

		logger.Info("Destroying project", zap.String("project", project.Name))
		eventChannel := make(chan events.EngineEvent)
		go processEvents(logger, eventChannel)

		s, err := createOrSelectStack(ctx, stack, project, source)
		if err != nil {
			logger.Error("Failed to create or select stack", zap.Error(err))
			return err
		}

		var destroyErr error
		if *jsonLogging {
			_, destroyErr = s.Destroy(ctx, optdestroy.EventStreams(eventChannel))
		} else {
			_, destroyErr = s.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
		}
		if destroyErr != nil {
			logger.Error("Failed to destroy project", zap.String("project", project.Name), zap.Error(destroyErr))
		} else {
			logger.Info("Successfully destroyed project", zap.String("project", project.Name))
		}
		return destroyErr
	}

	// Reverse the order of projects for destruction
	for i := len(projects) - 1; i >= 0; i-- {
		if err := destroyProject(projects[i]); err != nil {
			logger.Error("Failed to destroy project", zap.String("project", projects[i].Name), zap.Error(err))
			return
		}
	}

	logger.Info("Completed destruction")
}

func contains(slice []string, str string) bool {
	for _, v := range slice {
		if v == str {
			return true
		}
	}
	return false
}

func getValidStacks(requestedStacks []string, projects []Project) []string {
	validStacks := make(map[string]bool)
	for _, project := range projects {
		if len(project.AllowedStacks) == 0 {
			// If a project has no restrictions, all stacks are valid
			for _, stack := range requestedStacks {
				validStacks[stack] = true
			}
		} else {
			for _, allowedStack := range project.AllowedStacks {
				if contains(requestedStacks, allowedStack) {
					validStacks[allowedStack] = true
				}
			}
		}
	}

	result := make([]string, 0, len(validStacks))
	for stack := range validStacks {
		result = append(result, stack)
	}
	return result
}

func main() {
	kingpin.Version("0.0.1")

	startTime := time.Now()

	var wg sync.WaitGroup
	stackLock := &sync.Mutex{}
	activeStacks := make(map[string]bool)

	logger := createOutputLogger()
	defer logger.Sync()

	var operationType string
	var projects []Project

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case deployCmd.FullCommand(), destroyCmd.FullCommand():
		// Check if both git-url and local-path are specified
		if *gitRepoURL != "" && *localPath != "" {
			logger.Fatal("Error: Both --git-url and --local-path are specified. Please provide only one source.")
		}

		// Check if neither git-url nor local-path is specified
		if *gitRepoURL == "" && *localPath == "" {
			logger.Fatal("Error: Neither --git-url nor --local-path is specified. Please provide one source.")
		}

		source := ProjectSource{
			IsGit:     *gitRepoURL != "",
			GitURL:    *gitRepoURL,
			GitBranch: *gitBranch,
			LocalPath: *localPath,
		}

		// TODO: Implement logic to fetch projects dynamically based on source
		// For now, we'll use a placeholder implementation
		projects = []Project{
			{Name: "vpc", Path: "vpcs", AllowedStacks: nil},
			{Name: "eks", Path: "eks", AllowedStacks: nil},
			{Name: "monitoring", Path: "monitoring", AllowedStacks: nil},
			{Name: "demo-streamer", Path: "demo-streamer", AllowedStacks: []string{"west"}},
			{Name: "session-recorder", Path: "session-recorder", AllowedStacks: []string{"west"}},
		}

		validStacks := getValidStacks(*stacks, projects)

		if kingpin.MustParse(app.Parse(os.Args[1:])) == deployCmd.FullCommand() {
			operationType = "deploy"
			logger.Info("Starting deployment", zap.Strings("stacks", validStacks), zap.Any("projects", projects))

			wg.Add(len(validStacks))
			for _, stack := range validStacks {
				go func(stack string) {
					defer wg.Done()
					stackLock.Lock()
					if activeStacks[stack] {
						stackLock.Unlock()
						return
					}
					activeStacks[stack] = true
					stackLock.Unlock()

					deploy(stack, projects, source)

					stackLock.Lock()
					delete(activeStacks, stack)
					stackLock.Unlock()
				}(stack)
			}
		} else {
			operationType = "destroy"
			logger.Info("Starting destruction", zap.Strings("stacks", validStacks), zap.Any("projects", projects))

			wg.Add(len(validStacks))
			for _, stack := range validStacks {
				go func(stack string) {
					defer wg.Done()
					stackLock.Lock()
					if activeStacks[stack] {
						stackLock.Unlock()
						return
					}
					activeStacks[stack] = true
					stackLock.Unlock()

					destroy(stack, projects, source)

					stackLock.Lock()
					delete(activeStacks, stack)
					stackLock.Unlock()
				}(stack)
			}
		}
	}

	wg.Wait()

	duration := time.Since(startTime)
	logger.Info("Operation completed",
		zap.String("operation_type", operationType),
		zap.Duration("total_time", duration))
}
