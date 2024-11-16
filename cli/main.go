package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"encoding/json"
	"gopkg.in/yaml.v3"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Project represents a project in the YAML file.
type Project struct {
	Name      string   `yaml:"name"`
	Stacks    []string `yaml:"stacks"`
	DependsOn []string `yaml:"dependsOn"`
}

type ProjectSource struct {
	IsGit     bool
	GitURL    string
	GitBranch string
	LocalPath string
}

type Config struct {
	Projects []Project `yaml:"projects"`
}

var (
	app        = kingpin.New("demo-env-deployer", "A command-line application deployment tool using pulumi.")
	deployCmd  = app.Command("deploy", "Deploy the demo env.")
	destroyCmd = app.Command("destroy", "Destroy the demo env.")

	gitRepoURL  = app.Flag("git-url", "URL of the Git repository").String()
	gitBranch   = app.Flag("git-branch", "Git branch to use").Default("main").String()
	localPath   = app.Flag("path", "Path to local directory containing projects").String()
	configFile  = app.Flag("config", "Path to the YAML configuration file").Default("projects.yaml").String()
	jsonLogging = app.Flag("json", "Enable JSON logging").Bool()
	org         = app.Flag("org", "Organization to deploy to").String()
)

func loadConfig(configPath string) ([]Project, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg.Projects, nil
}

func createOrSelectStack(ctx context.Context, org string, stackName string, project Project, source ProjectSource) (auto.Stack, error) {
	var usedStackName string
	if org == "" {
		usedStackName = stackName
	} else {
		usedStackName = org + "/" + stackName
	}
	projectPath := filepath.Join(source.LocalPath, project.Name)
	return auto.UpsertStackLocalSource(ctx, usedStackName, projectPath)
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

func destroy(org string, projects []Project, source ProjectSource) {
	logger := createOutputLogger()
	defer logger.Sync()

	logger.Info("Starting destruction")

	ctx := context.Background()

	destroyStack := func(project Project, stack string) error {
		logger := logger.With(zap.String("project", project.Name), zap.String("stack", stack))
		logger.Info("Destroying stack")

		eventChannel := make(chan events.EngineEvent)
		go processEvents(logger, eventChannel)

		s, err := createOrSelectStack(ctx, org, stack, project, source)
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
			logger.Error("Failed to destroy stack", zap.Error(destroyErr))
		} else {
			logger.Info("Successfully destroyed stack")
		}
		return destroyErr
	}

	destroyProject := func(project Project) {
		logger.Info("Destroying project", zap.String("project", project.Name))

		// Destroy stacks in parallel
		var stackWG sync.WaitGroup
		for _, stack := range project.Stacks {
			stackWG.Add(1)
			go func(stack string) {
				defer stackWG.Done()
				if err := destroyStack(project, stack); err != nil {
					logger.Error("Failed to destroy stack", zap.String("stack", stack), zap.Error(err))
				}
			}(stack)
		}
		stackWG.Wait()

		logger.Info("Completed destruction for project", zap.String("project", project.Name))
	}

	// Reverse order for destruction
	for i := len(projects) - 1; i >= 0; i-- {
		project := projects[i]
		destroyProject(project)
	}

	logger.Info("Completed destruction for all projects")
}






func deployStack(project Project, stack string, org string, source ProjectSource, ctx context.Context, logger *zap.Logger) error {
	logger = logger.With(zap.String("project", project.Name), zap.String("stack", stack))
	logger.Info("Deploying stack")

	eventChannel := make(chan events.EngineEvent)
	go processEvents(logger, eventChannel)

	// Use createOrSelectStack to ensure the stack name includes the org prefix
	s, err := createOrSelectStack(ctx, org, stack, project, source)
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
		logger.Error("Failed to deploy stack", zap.Error(upErr))
	} else {
		logger.Info("Successfully deployed stack")
	}
	return upErr
}

func deploy(org string, projects []Project, source ProjectSource) {
	logger := createOutputLogger()
	defer logger.Sync()

	logger.Info("Starting deployment")

	ctx := context.Background()
	deployed := make(map[string]bool) // Tracks completed projects
	mu := &sync.Mutex{}

	for _, project := range projects {
		logger.Info("Checking dependencies for project", zap.String("project", project.Name))

		// Check if all dependencies are satisfied
		mu.Lock()
		for _, dep := range project.DependsOn {
			if !deployed[dep] {
				mu.Unlock()
				logger.Fatal("Dependency not satisfied", zap.String("project", project.Name), zap.String("dependency", dep))
			}
		}
		mu.Unlock()

		logger.Info("Deploying project", zap.String("project", project.Name))

		// Deploy stacks in parallel
		var stackWG sync.WaitGroup
		for _, stack := range project.Stacks {
			stackWG.Add(1)
			go func(stack string) {
				defer stackWG.Done()
				if err := deployStack(project, stack, org, source, ctx, logger); err != nil {
					logger.Error("Failed to deploy stack", zap.String("stack", stack), zap.Error(err))
				}
			}(stack)
		}
		stackWG.Wait()

		// Mark project as deployed
		mu.Lock()
		deployed[project.Name] = true
		mu.Unlock()

		logger.Info("Completed deployment for project", zap.String("project", project.Name))
	}

	logger.Info("Completed deployment for all projects")
}

func validateDependencies(projects []Project) error {
	// Build a set of valid project names
	projectNames := make(map[string]struct{})
	for _, project := range projects {
		projectNames[project.Name] = struct{}{}
	}

	// Check dependencies for each project
	for _, project := range projects {
		for _, dep := range project.DependsOn {
			if _, exists := projectNames[dep]; !exists {
				return fmt.Errorf("project %q depends on missing project %q", project.Name, dep)
			}
		}
	}
	return nil
}


func main() {
	kingpin.Version("0.0.1")

	startTime := time.Now()

	logger := createOutputLogger()
	defer logger.Sync()

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case deployCmd.FullCommand(), destroyCmd.FullCommand():
		source := ProjectSource{
			IsGit:     *gitRepoURL != "",
			GitURL:    *gitRepoURL,
			GitBranch: *gitBranch,
			LocalPath: *localPath,
		}

		projects, err := loadConfig(*configFile)
		if err != nil {
			logger.Fatal("Failed to load configuration", zap.Error(err))
		}

		// Validate dependencies
		if err := validateDependencies(projects); err != nil {
			logger.Fatal("Invalid dependency graph", zap.Error(err))
		}

		if kingpin.MustParse(app.Parse(os.Args[1:])) == deployCmd.FullCommand() {
			deploy(*org, projects, source)
		} else {
			destroy(*org, projects, source)
		}
	}

	duration := time.Since(startTime)
	logger.Info("Operation completed",
		zap.Duration("total_time", duration))
}

