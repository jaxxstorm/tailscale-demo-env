package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"encoding/json"
	"github.com/alecthomas/kingpin/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

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
	return zap.New(core)
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

	ctx := context.Background()

	// Deploy VPC stack
	vpcEventChannel := make(chan events.EngineEvent)
	go processEvents(logger, vpcEventChannel)
	vpcStack := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/vpcs", *path))
	var err error
	if *jsonLogging {
		_, err = vpcStack.Up(ctx, optup.EventStreams(vpcEventChannel))
	} else {
		_, err = vpcStack.Up(ctx, optup.ProgressStreams(os.Stdout))
	}
	if err != nil {
		if *jsonLogging {
			logger.Error("Failed to update VPC stack", zap.Error(err))
		} else {
			fmt.Printf("Failed to update VPC stack: %v\n", err)
		}
		os.Exit(1)
	}

	// Deploy EKS stack
	eksEventChannel := make(chan events.EngineEvent)
	go processEvents(logger, eksEventChannel)
	eksStack := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/eks", *path))
	if *jsonLogging {
		_, err = eksStack.Up(ctx, optup.EventStreams(eksEventChannel))
	} else {
		_, err = eksStack.Up(ctx, optup.ProgressStreams(os.Stdout))
	}
	if err != nil {
		if *jsonLogging {
			logger.Error("Failed to update EKS stack", zap.Error(err))
		} else {
			fmt.Printf("Failed to update EKS stack: %v\n", err)
		}
		os.Exit(1)
	}
}

func destroy(stack string) {
	logger := createOutputLogger().With(zap.String("stack", stack))
	defer logger.Sync()

	ctx := context.Background()

	// Destroy EKS stack
	eksEventChannel := make(chan events.EngineEvent)
	go processEvents(logger, eksEventChannel)
	eksStack := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/eks", *path))
	var err error
	if *jsonLogging {
		_, err = eksStack.Destroy(ctx, optdestroy.EventStreams(eksEventChannel))
	} else {
		_, err = eksStack.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
	}
	if err != nil {
		if *jsonLogging {
			logger.Error("Failed to destroy EKS stack", zap.Error(err))
		} else {
			fmt.Printf("Failed to destroy EKS stack: %v\n", err)
		}
		os.Exit(1)
	}

	// Destroy VPC stack
	vpcEventChannel := make(chan events.EngineEvent)
	go processEvents(logger, vpcEventChannel)
	vpcStack := createOrSelectStack(ctx, stack, fmt.Sprintf("%s/vpcs", *path))
	if *jsonLogging {
		_, err = vpcStack.Destroy(ctx, optdestroy.EventStreams(vpcEventChannel))
	} else {
		_, err = vpcStack.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
	}
	if err != nil {
		if *jsonLogging {
			logger.Error("Failed to destroy VPC stack", zap.Error(err))
		} else {
			fmt.Printf("Failed to destroy VPC stack: %v\n", err)
		}
		os.Exit(1)
	}
}

func main() {
	kingpin.Version("0.0.1")

	var wg sync.WaitGroup

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case deployCmd.FullCommand():
		wg.Add(len(*stacks))

		for _, stack := range *stacks {
			stack := stack
			go func(stack string) {
				deploy(stack)
				wg.Done()
			}(stack)
		}

	case destroyCmd.FullCommand():
		wg.Add(len(*stacks))
		for _, stack := range *stacks {
			stack := stack
			go func(stack string) {
				destroy(stack)
				wg.Done()
			}(stack)
		}
	}

	wg.Wait()
}
