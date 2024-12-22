package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// コマンドライン フラグ
var (
	stackName  = flag.String("stack", "", "CloudFormation stack name (required)")
	profile    = flag.String("profile", "", "AWS CLI profile name (optional)")
	cdkAppPath = flag.String("cdk-app-path", "", "Full path to the CDK app entry file, e.g. /path/to/bin/app.ts (required)")
	cdkAppRoot = flag.String("cdk-app-root", ".", "CDK project root path (where cdk.json is). Defaults to current directory.")
)

func main() {
	flag.Parse()

	if *stackName == "" {
		log.Fatal("Error: --stack を指定してください。")
	}
	if *cdkAppPath == "" {
		log.Fatal("Error: --cdk-app-path を指定してください。")
	}

	ctx := context.Background()

	// AWS Config をロード (profile のみ反映、region 引数は省略)
	cfg, err := loadAWSConfig(ctx, *profile)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	// 1. ECS クラスター名の取得
	clusterName, err := getEcsClusterNameFromStack(ctx, cfg, *stackName)
	if err != nil {
		log.Fatalf("Failed to get ECS cluster name: %v", err)
	}
	if clusterName == "" {
		log.Printf("No ECS::Cluster in stack: %s", *stackName)
	} else {
		// 2. ECSサービスを停止・削除
		if err := deleteEcsServices(ctx, cfg, clusterName); err != nil {
			log.Fatalf("Failed to delete ECS services: %v", err)
		}
		// 3. タスクを停止
		if err := stopRemainingTasks(ctx, cfg, clusterName); err != nil {
			log.Fatalf("Failed to stop tasks: %v", err)
		}
	}

	// 4. cdk destroy (--all) 実行
	if err := runCdkDestroy(*profile, *cdkAppRoot, *cdkAppPath); err != nil {
		log.Fatalf("Failed to run cdk destroy: %v", err)
	}
	log.Println("All done.")
}

// AWS Config ロード (profile のみ考慮)
func loadAWSConfig(ctx context.Context, profile string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// CloudFormation から ECS Cluster名を取得
func getEcsClusterNameFromStack(ctx context.Context, cfg aws.Config, stackName string) (string, error) {
	cfnClient := cfn.NewFromConfig(cfg)
	res, err := cfnClient.ListStackResources(ctx, &cfn.ListStackResourcesInput{
		StackName: &stackName,
	})
	if err != nil {
		return "", err
	}

	for _, r := range res.StackResourceSummaries {
		if r.ResourceType != nil && *r.ResourceType == "AWS::ECS::Cluster" {
			return *r.PhysicalResourceId, nil
		}
	}
	return "", nil
}

// ECSサービスを停止（DesiredCount=0）→ 削除
func deleteEcsServices(ctx context.Context, cfg aws.Config, clusterName string) error {
	ecsClient := ecs.NewFromConfig(cfg)

	listOut, err := ecsClient.ListServices(ctx, &ecs.ListServicesInput{
		Cluster: &clusterName,
	})
	if err != nil {
		return fmt.Errorf("ListServices error: %w", err)
	}

	if len(listOut.ServiceArns) == 0 {
		log.Printf("No ECS services found in cluster: %s", clusterName)
		return nil
	}

	for _, svcArn := range listOut.ServiceArns {
		svcName := arnToName(svcArn)
		log.Printf("[Service: %s] Setting desired count to 0...", svcName)

		_, err := ecsClient.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster:      &clusterName,
			Service:      &svcName,
			DesiredCount: aws.Int32(0),
		})
		if err != nil {
			log.Printf("Failed to update service(%s) desiredCount=0: %v", svcName, err)
			continue
		}

		if err := waitForServiceStable(ctx, ecsClient, clusterName, svcName); err != nil {
			log.Printf("waitForServiceStable failed for service(%s): %v", svcName, err)
		}

		log.Printf("[Service: %s] Deleting...", svcName)
		_, err = ecsClient.DeleteService(ctx, &ecs.DeleteServiceInput{
			Cluster: &clusterName,
			Service: &svcName,
			Force:   aws.Bool(true),
		})
		if err != nil {
			log.Printf("Failed to delete service(%s): %v", svcName, err)
		}
	}
	return nil
}

// クラスターに残っているタスクを停止
func stopRemainingTasks(ctx context.Context, cfg aws.Config, clusterName string) error {
	ecsClient := ecs.NewFromConfig(cfg)

	listOut, err := ecsClient.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       &clusterName,
		DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return fmt.Errorf("ListTasks error: %w", err)
	}
	if len(listOut.TaskArns) == 0 {
		log.Printf("No running tasks in cluster: %s", clusterName)
		return nil
	}

	for _, taskArn := range listOut.TaskArns {
		taskName := arnToName(taskArn)
		log.Printf("[Task: %s] Stopping...", taskName)
		_, err := ecsClient.StopTask(ctx, &ecs.StopTaskInput{
			Cluster: &clusterName,
			Task:    &taskArn,
			Reason:  aws.String("Cleanup before destroy"),
		})
		if err != nil {
			log.Printf("Failed to stop task(%s): %v", taskName, err)
		}
	}
	return nil
}

// cdk destroy 実行
func runCdkDestroy(profile, cdkAppRoot, cdkAppPath string) error {
	args := []string{"destroy", "--all", "--force"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}

	// --app 引数
	appArg := fmt.Sprintf("npx ts-node %s", cdkAppPath)
	args = append(args, "--app", appArg)

	log.Printf("Executing: cdk %s", strings.Join(args, " "))

	// コマンド作成
	cmd := exec.Command("cdk", args...)

	// ★ ポイント: カレントディレクトリをCDKアプリのルートに変更
	cmd.Dir = cdkAppRoot

	// 出力を標準出力・標準エラーに紐付け
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 実行
	return cmd.Run()
}

// ARN末尾からリソース名を取り出す
func arnToName(arn string) string {
	parts := strings.Split(arn, "/")
	return parts[len(parts)-1]
}

// サービスが STABLE になるまで待機
func waitForServiceStable(ctx context.Context, ecsClient *ecs.Client, clusterName, serviceName string) error {
	svcWaiter := ecs.NewServicesStableWaiter(ecsClient)
	input := &ecs.DescribeServicesInput{
		Cluster:  &clusterName,
		Services: []string{serviceName},
	}
	maxWait := 10 * time.Minute
	return svcWaiter.Wait(ctx, input, maxWait)
}
