package main

import (
	"context"
	"fmt"
	"os"
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
    Name         string
    Path         string
    AllowedStacks []string // New field to specify allowed stacks
}

var projects = map[string]Project{
    "vpc":              {Name: "vpc", Path: "vpcs", AllowedStacks: nil},
    "eks":              {Name: "eks", Path: "eks", AllowedStacks: nil},
    "monitoring":       {Name: "monitoring", Path: "monitoring", AllowedStacks: nil},
    "demo-streamer":    {Name: "demo-streamer", Path: "demo-streamer", AllowedStacks: []string{"west"}},
    "session-recorder": {Name: "session-recorder", Path: "session-recorder", AllowedStacks: []string{"west"}},
}

var (
	app        = kingpin.New("demo-env-deployer", "A command-line application deployment tool using pulumi.")
	deployCmd  = app.Command("deploy", "Deploy the demo env.")
	destroyCmd = app.Command("destroy", "Destroy the demo env.")

	path        = app.Flag("path", "Path to demo env directory").Default(".").String()
	jsonLogging = app.Flag("json", "Enable JSON logging").Bool()
	stacks      = app.Flag("stacks", "Stacks to deploy").Default("west", "east", "eu").Strings()
)

func createOrSelectStack(ctx context.Context, stackName, projectPath string) auto.Stack {

	s, err := auto.UpsertStackLocalSource(ctx, stackName, projectPath)
	if err != nil {
		fmt.Printf("Failed to create or select stack: %v\n", err)
		os.Exit(1)
	}

	return s

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

func deploy(stack string) {
    logger := createOutputLogger().With(zap.String("stack", stack))
    defer logger.Sync()

    logger.Info(fmt.Sprintf("Starting deployment for stack: %s", stack))

    ctx := context.Background()

    deployProject := func(project Project) error {
        if len(project.AllowedStacks) > 0 && !contains(project.AllowedStacks, stack) {
            logger.Info(fmt.Sprintf("Skipping %s project for stack %s (not in allowed stacks)", project.Name, stack))
            return nil
        }

        logger.Info(fmt.Sprintf("Deploying %s project for stack %s", project.Name, stack))
        eventChannel := make(chan events.EngineEvent)
        go processEvents(logger, eventChannel)
        s := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/%s", *path, project.Path))
        var err error
        if *jsonLogging {
            _, err = s.Up(ctx, optup.EventStreams(eventChannel))
        } else {
            _, err = s.Up(ctx, optup.ProgressStreams(os.Stdout))
        }
        if err != nil {
            logger.Error(fmt.Sprintf("Failed to update %s project for stack %s", project.Name, stack), zap.Error(err))
        } else {
            logger.Info(fmt.Sprintf("Successfully deployed %s project for stack %s", project.Name, stack))
        }
        return err
    }

    deployOrder := []string{"vpc", "eks", "monitoring", "demo-streamer", "session-recorder"}

    for _, projectName := range deployOrder {
        project, exists := projects[projectName]
        if !exists {
            logger.Error(fmt.Sprintf("Unknown project type: %s", projectName))
            continue
        }
        if err := deployProject(project); err != nil {
            logger.Error(fmt.Sprintf("Failed to deploy %s project for stack %s", project.Name, stack), zap.Error(err))
            return
        }
    }

    logger.Info(fmt.Sprintf("Completed deployment for stack: %s", stack))
}

func contains(slice []string, str string) bool {
    for _, v := range slice {
        if v == str {
            return true
        }
    }
    return false
}

func destroy(stack string) {
    logger := createOutputLogger().With(zap.String("stack", stack))
    defer logger.Sync()

    logger.Info(fmt.Sprintf("Starting destruction for stack: %s", stack))

    ctx := context.Background()

    destroyProject := func(project Project) error {
        if len(project.AllowedStacks) > 0 && !contains(project.AllowedStacks, stack) {
            logger.Info(fmt.Sprintf("Skipping %s project for stack %s (not in allowed stacks)", project.Name, stack))
            return nil
        }

        logger.Info(fmt.Sprintf("Destroying %s project for stack %s", project.Name, stack))
        eventChannel := make(chan events.EngineEvent)
        go processEvents(logger, eventChannel)
        s := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/%s", *path, project.Path))
        var err error
        if *jsonLogging {
            _, err = s.Destroy(ctx, optdestroy.EventStreams(eventChannel))
        } else {
            _, err = s.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
        }
        if err != nil {
            logger.Error(fmt.Sprintf("Failed to destroy %s project for stack %s", project.Name, stack), zap.Error(err))
        } else {
            logger.Info(fmt.Sprintf("Successfully destroyed %s project for stack %s", project.Name, stack))
        }
        return err
    }

    // Define the order of destruction (reverse of deployment order)
    destroyOrder := []string{"session-recorder", "demo-streamer", "monitoring", "eks", "vpc"}

    for _, projectName := range destroyOrder {
        project, exists := projects[projectName]
        if !exists {
            logger.Error(fmt.Sprintf("Unknown project type: %s", projectName))
            continue
        }
        if err := destroyProject(project); err != nil {
            logger.Error(fmt.Sprintf("Failed to destroy %s project for stack %s", project.Name, stack), zap.Error(err))
            return
        }
    }

    logger.Info(fmt.Sprintf("Completed destruction for stack: %s", stack))
}

func getValidStacks(requestedStacks []string) []string {
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

    var wg sync.WaitGroup
    stackLock := &sync.Mutex{}
    activeStacks := make(map[string]bool)

    switch kingpin.MustParse(app.Parse(os.Args[1:])) {
    case deployCmd.FullCommand():
        validStacks := getValidStacks(*stacks)
        fmt.Printf("Valid stacks for deployment: %v\n", validStacks)
        
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
                
                deploy(stack)
                
                stackLock.Lock()
                delete(activeStacks, stack)
                stackLock.Unlock()
            }(stack)
        }

    case destroyCmd.FullCommand():
        validStacks := getValidStacks(*stacks)
        fmt.Printf("Valid stacks for destruction: %v\n", validStacks)
        
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
                
                destroy(stack)
                
                stackLock.Lock()
                delete(activeStacks, stack)
                stackLock.Unlock()
            }(stack)
        }
    }

    wg.Wait()
}
