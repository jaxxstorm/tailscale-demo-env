package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"encoding/json"
	"github.com/dominikbraun/graph"
	"gopkg.in/yaml.v3"

	"github.com/alecthomas/kingpin/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170")).
			PaddingBottom(1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			PaddingBottom(1)

	stepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("36")).
			Width(8)

	itemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

var (
	app        = kingpin.New("demo-env-deployer", "A command-line application deployment tool using pulumi.")
	deployCmd  = app.Command("deploy", "Deploy the demo env.")
	destroyCmd = app.Command("destroy", "Destroy the demo env.")

	gitRepoURL     = app.Flag("git-url", "URL of the Git repository").String()
	gitBranch      = app.Flag("git-branch", "Git branch to use").Default("main").String()
	localPath      = app.Flag("path", "Path to local directory containing projects").String()
	configFile     = app.Flag("config", "Path to the YAML configuration file").Default("projects.yaml").String()
	jsonLogging    = app.Flag("json", "Enable JSON logging").Bool()
	deployPreview  = deployCmd.Flag("preview", "Preview the deployment plan").Bool()
	destroyPreview = destroyCmd.Flag("preview", "Preview the destruction plan").Bool()
	org            = app.Flag("org", "Organization to deploy to").String()
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

func preview(projects []Project, mode string) error {
	// Get execution groups
	executionGroups, err := getExecutionGroups(projects)
	if err != nil {
		return fmt.Errorf("failed to determine execution groups: %w", err)
	}

	groups := executionGroups
	if mode == "destroy" {
		groups = reverseGroups(executionGroups)
	}

	// Find the longest item for proper spacing
	maxWidth := 0
	for _, group := range groups {
		for _, item := range group {
			if len(item) > maxWidth {
				maxWidth = len(item)
			}
		}
	}

	// Adjust separator width to match content
	separatorWidth := maxWidth + 12 // 8 for step column + 4 for spacing
	separator := separatorStyle.Render(strings.Repeat("─", separatorWidth))

	// Create title using proper case handling
	caser := cases.Title(language.English)
	title := titleStyle.Render(fmt.Sprintf("%s Plan:", caser.String(mode)))
	fmt.Println("\n" + title)

	// Rest of the function remains the same...
	header := lipgloss.JoinHorizontal(
		lipgloss.Left,
		headerStyle.Width(8).Render("Step"),
		headerStyle.Render("Stacks"),
	)
	fmt.Println(header)
	fmt.Println(separator)

	// Print each group
	for i, group := range groups {
		stepNum := i + 1
		if mode == "destroy" {
			stepNum = len(groups) - i
		}

		// Create step number
		step := stepStyle.Render(fmt.Sprintf("Step %d", stepNum))

		// Format items
		sort.Strings(group) // Sort items for consistent output
		for j, item := range group {
			if j == 0 {
				// First item goes on the same line as the step
				row := lipgloss.JoinHorizontal(
					lipgloss.Left,
					step,
					itemStyle.Render(item),
				)
				fmt.Println(row)
			} else {
				// Subsequent items are indented to align with the first item
				fmt.Println(lipgloss.JoinHorizontal(
					lipgloss.Left,
					strings.Repeat(" ", 8),
					itemStyle.Render(item),
				))
			}
		}

		if i < len(groups)-1 {
			fmt.Println(separator)
		}
	}

	fmt.Println(separator)
	fmt.Println() // Add final newline
	return nil
}

func reverseGroups(groups [][]string) [][]string {
	reversed := make([][]string, len(groups))
	for i := 0; i < len(groups); i++ {
		reversed[i] = groups[len(groups)-1-i]
	}
	return reversed
}

// Creates a unique vertex ID for a project and stack combination
func vertexID(project, stack string) string {
	return fmt.Sprintf("%s:%s", project, stack)
}

func containsStack(stacks []string, stack string) bool {
	for _, s := range stacks {
		if s == stack {
			return true
		}
	}
	return false
}

// Get execution groups that can run concurrently
func getExecutionGroups(projects []Project) ([][]string, error) {
	// Create a directed graph
	g := graph.New(graph.StringHash, graph.Directed())

	// Track dependencies for each vertex
	dependencies := make(map[string][]string)

	// First, create a map of all valid project-stack combinations and merge duplicate projects
	validStacks := make(map[string][]string)
	projectDeps := make(map[string][]string) // Track merged dependencies

	for _, project := range projects {
		// Merge stacks for duplicate projects
		if existing, ok := validStacks[project.Name]; ok {
			// Create a map for unique stacks
			stackMap := make(map[string]bool)
			for _, s := range existing {
				stackMap[s] = true
			}
			for _, s := range project.Stacks {
				stackMap[s] = true
			}

			// Convert back to slice
			var mergedStacks []string
			for s := range stackMap {
				mergedStacks = append(mergedStacks, s)
			}
			validStacks[project.Name] = mergedStacks

			// Merge dependencies
			depMap := make(map[string]bool)
			for _, dep := range projectDeps[project.Name] {
				depMap[dep] = true
			}
			for _, dep := range project.DependsOn {
				depMap[dep] = true
			}

			var mergedDeps []string
			for dep := range depMap {
				mergedDeps = append(mergedDeps, dep)
			}
			projectDeps[project.Name] = mergedDeps
		} else {
			validStacks[project.Name] = project.Stacks
			projectDeps[project.Name] = project.DependsOn
		}
	}

	// Add all vertices first (project:stack combinations)
	for projectName, stacks := range validStacks {
		for _, stack := range stacks {
			vertex := vertexID(projectName, stack)
			if err := g.AddVertex(vertex); err != nil {
				return nil, fmt.Errorf("failed to add vertex %s: %w", vertex, err)
			}
		}
	}

	// Add edges for dependencies
	for projectName, stacks := range validStacks {
		deps := projectDeps[projectName]
		for _, stack := range stacks {
			currentVertex := vertexID(projectName, stack)
			dependencies[currentVertex] = []string{}

			// Add edges for each dependency, but only if the dependency exists in the same stack
			for _, dep := range deps {
				// Check if the dependency exists in this stack
				if containsStack(validStacks[dep], stack) {
					depVertex := vertexID(dep, stack)
					if err := g.AddEdge(depVertex, currentVertex); err != nil {
						return nil, fmt.Errorf("failed to add edge from %s to %s: %w", depVertex, currentVertex, err)
					}
					dependencies[currentVertex] = append(dependencies[currentVertex], depVertex)
				}
			}
		}
	}

	// Get vertices in topological order
	order, err := graph.TopologicalSort(g)
	if err != nil {
		return nil, fmt.Errorf("failed to perform topological sort: %w", err)
	}

	// Create concurrent execution groups
	var executionGroups [][]string
	processed := make(map[string]bool)

	// Process all vertices
	for len(processed) < len(order) {
		var currentGroup []string

		// Find all vertices that can be executed
		for _, vertex := range order {
			if processed[vertex] {
				continue
			}

			// Check if all dependencies are processed
			canExecute := true
			for _, dep := range dependencies[vertex] {
				if !processed[dep] {
					canExecute = false
					break
				}
			}

			if canExecute {
				currentGroup = append(currentGroup, vertex)
			}
		}

		// Sort the group for consistent output
		sort.Strings(currentGroup)

		// Mark all vertices in current group as processed
		for _, vertex := range currentGroup {
			processed[vertex] = true
		}

		executionGroups = append(executionGroups, currentGroup)
	}

	return executionGroups, nil
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

func createOutputLogger(fields ...zap.Field) *zap.Logger {
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

	// Add global fields to the logger
	return zap.New(sampling).With(fields...)
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

func deployStack(project Project, stack string, org string, source ProjectSource, ctx context.Context, logger *zap.Logger) error {
	logger = logger.With(zap.String("project", project.Name), zap.String("stack", stack))
	logger.Info("Deploying stack")

	eventChannel := make(chan events.EngineEvent)
	go processEvents(logger, eventChannel)

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
	// Create a logger with a global field for deployment
	logger := createOutputLogger(zap.String("operation", "deploy"))
	defer logger.Sync()

	logger.Info("Starting deployment")

	// Get execution groups
	executionGroups, err := getExecutionGroups(projects)
	if err != nil {
		logger.Fatal("Failed to determine execution groups", zap.Error(err))
	}

	// Log the execution schedule
	logger.Info("Execution Schedule")
	for i, group := range executionGroups {
		logger.Info("Deployment Stage",
			zap.Int("stage", i+1),
			zap.Strings("deployments", group))
	}

	ctx := context.Background()
	deployed := make(map[string]bool)
	mu := &sync.Mutex{}

	// Execute each group sequentially
	for groupIndex, group := range executionGroups {
		stageLogger := logger.With(zap.Int("stage", groupIndex+1))
		stageLogger.Info("Executing deployment stage")

		var groupWG sync.WaitGroup
		groupErrors := make(chan error, len(group))

		// Deploy all items in the group concurrently
		for _, vertex := range group {
			groupWG.Add(1)
			go func(vertex string) {
				defer groupWG.Done()

				// Parse project and stack from vertex ID
				parts := strings.Split(vertex, ":")
				projectName, stackName := parts[0], parts[1]

				// Find the project definition
				var projectDef Project
				for _, p := range projects {
					if p.Name == projectName {
						projectDef = p
						break
					}
				}

				// Deploy the stack
				stackLogger := stageLogger.With(
					zap.String("project", projectName),
					zap.String("stack", stackName),
				)
				stackLogger.Info("Deploying stack")
				err := deployStack(projectDef, stackName, org, source, ctx, stackLogger)
				if err != nil {
					groupErrors <- fmt.Errorf("failed to deploy %s: %w", vertex, err)
					return
				}

				// Mark as deployed
				mu.Lock()
				deployed[vertex] = true
				mu.Unlock()
			}(vertex)
		}

		// Wait for all deployments in this group to complete
		groupWG.Wait()
		close(groupErrors)

		// Check for any errors in this group
		for err := range groupErrors {
			if err != nil {
				stageLogger.Fatal("Deployment failed", zap.Error(err))
			}
		}

		stageLogger.Info("Completed deployment stage")
	}

	logger.Info("Deployment completed successfully")
}

func destroy(org string, projects []Project, source ProjectSource) {
	logger := createOutputLogger(zap.String("operation", "destroy"))
	defer logger.Sync()

	logger.Info("Starting destruction")

	// Get execution groups
	executionGroups, err := getExecutionGroups(projects)
	if err != nil {
		logger.Fatal("Failed to determine execution groups", zap.Error(err))
	}

	// Print destruction plan (in reverse order)
	logger.Info("Destruction Schedule:")
	for i := len(executionGroups) - 1; i >= 0; i-- {
		stageLogger := logger.With(zap.Int("stage", len(executionGroups)-i))
		stageLogger.Info(fmt.Sprintf("Step %d (these will be destroyed concurrently):", len(executionGroups)-i),
			zap.Strings("destructions", executionGroups[i]))
	}

	ctx := context.Background()
	destroyed := make(map[string]bool)
	mu := &sync.Mutex{}

	// Execute each group sequentially in reverse order
	for i := len(executionGroups) - 1; i >= 0; i-- {
		group := executionGroups[i]
		stageLogger := logger.With(zap.Int("stage", len(executionGroups)-i))
		stageLogger.Info(fmt.Sprintf("Executing Destruction Step %d", len(executionGroups)-i))

		var groupWG sync.WaitGroup
		groupErrors := make(chan error, len(group))

		// Destroy all items in the group concurrently
		for _, vertex := range group {
			groupWG.Add(1)
			go func(vertex string) {
				defer groupWG.Done()

				// Parse project and stack from vertex ID
				parts := strings.Split(vertex, ":")
				projectName, stackName := parts[0], parts[1]

				// Find the project definition
				var projectDef Project
				for _, p := range projects {
					if p.Name == projectName {
						projectDef = p
						break
					}
				}

				// Create event channel for this stack
				eventChannel := make(chan events.EngineEvent)
				go processEvents(stageLogger.With(
					zap.String("project", projectName),
					zap.String("stack", stackName),
				), eventChannel)

				// Create or select the stack
				s, err := createOrSelectStack(ctx, org, stackName, projectDef, source)
				if err != nil {
					groupErrors <- fmt.Errorf("failed to select stack %s: %w", vertex, err)
					return
				}

				// Destroy the stack
				var destroyErr error
				if *jsonLogging {
					_, destroyErr = s.Destroy(ctx, optdestroy.EventStreams(eventChannel))
				} else {
					_, destroyErr = s.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
				}

				if destroyErr != nil {
					groupErrors <- fmt.Errorf("failed to destroy %s: %w", vertex, destroyErr)
					return
				}

				// Mark as destroyed
				mu.Lock()
				destroyed[vertex] = true
				mu.Unlock()

				stageLogger.Info("Successfully destroyed stack",
					zap.String("project", projectName),
					zap.String("stack", stackName))
			}(vertex)
		}

		// Wait for all destructions in this group to complete
		groupWG.Wait()
		close(groupErrors)

		// Check for any errors in this group
		for err := range groupErrors {
			if err != nil {
				stageLogger.Fatal("Destruction failed", zap.Error(err))
			}
		}

		stageLogger.Info(fmt.Sprintf("Completed Destruction Step %d", len(executionGroups)-i))
	}

	logger.Info("Completed destruction for all projects")
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

	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))

	// Load and validate configuration first as it's needed for all paths
	projects, err := loadConfig(*configFile)
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Validate dependencies
	if err := validateDependencies(projects); err != nil {
		logger.Fatal("Invalid dependency graph", zap.Error(err))
	}

	// Set up source configuration
	source := ProjectSource{
		IsGit:     *gitRepoURL != "",
		GitURL:    *gitRepoURL,
		GitBranch: *gitBranch,
		LocalPath: *localPath,
	}

	switch cmd {
	case deployCmd.FullCommand():
		if *deployPreview {
			if err := preview(projects, "deploy"); err != nil {
				logger.Fatal("Failed to preview deployment plan", zap.Error(err))
			}
		} else {
			deploy(*org, projects, source)
		}

	case destroyCmd.FullCommand():
		if *destroyPreview {
			if err := preview(projects, "destroy"); err != nil {
				logger.Fatal("Failed to preview destruction plan", zap.Error(err))
			}
		} else {
			destroy(*org, projects, source)
		}
	}

	duration := time.Since(startTime)
	logger.Info("Operation completed",
		zap.Duration("total_time", duration))
}
