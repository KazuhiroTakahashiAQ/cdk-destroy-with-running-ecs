package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// 1) コマンドライン フラグに --cdk-app-file を追加。
var (
	stackName  = flag.String("stack", "", "CloudFormation stack name")
	profile    = flag.String("profile", "", "AWS CLI profile name")
	region     = flag.String("region", "us-east-1", "AWS region")
	cdkAppDir  = flag.String("cdk-app-dir", ".", "Root directory of the CDK app (contains bin/, lib/, test/, etc.)")
	cdkAppFile = flag.String("cdk-app-file", "app.ts", "CDK app entry file name (e.g., app.ts or main.ts)")
)

func main() {
	flag.Parse()
	if *stackName == "" {
		log.Fatal("Error: --stack を指定してください。")
	}

	ctx := context.Background()

	// AWS SDKのConfigを初期化
	cfg, err := loadAWSConfig(ctx, *profile, *region)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// 1. CloudFormationからECSクラスター名を取得
	clusterName, err := getEcsClusterNameFromStack(ctx, cfg, *stackName)
	if err != nil {
		log.Fatalf("failed to get ECS cluster name: %v", err)
	}

	if clusterName == "" {
		log.Printf("Stack %s 内に ECS::Cluster リソースが見つかりませんでした。", *stackName)
	} else {
		log.Printf("Detected ECS Cluster: %s", clusterName)

		// 2. ECSサービスの停止・削除
		if err := deleteEcsServices(ctx, cfg, clusterName); err != nil {
			log.Fatalf("failed to delete ECS services: %v", err)
		}

		// 3. クラスターに残っているタスクがあれば停止
		if err := stopRemainingTasks(ctx, cfg, clusterName); err != nil {
			log.Fatalf("failed to stop remaining tasks: %v", err)
		}
	}

	// 4. cdk destroy の実行
	if err := runCdkDestroy(*stackName, *profile, *region, *cdkAppDir, *cdkAppFile); err != nil {
		log.Fatalf("failed to run cdk destroy: %v", err)
	}

	log.Println("All done.")
}

// ============================================
// AWS Config ロード
// ============================================
func loadAWSConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// ============================================
// CloudFormation から ECS Cluster名を取得
// ============================================
func getEcsClusterNameFromStack(ctx context.Context, cfg aws.Config, stackName string) (string, error) {
	cfnClient := cfn.NewFromConfig(cfg)

	res, err := cfnClient.ListStackResources(ctx, &cfn.ListStackResourcesInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		return "", err
	}

	for _, r := range res.StackResourceSummaries {
		if r.ResourceType != nil && *r.ResourceType == "AWS::ECS::Cluster" {
			return aws.ToString(r.PhysicalResourceId), nil
		}
	}
	return "", nil
}

// ============================================
// ECSサービスを停止（DesiredCount=0）→ 削除
// ============================================
func deleteEcsServices(ctx context.Context, cfg aws.Config, clusterName string) error {
	ecsClient := ecs.NewFromConfig(cfg)

	// クラスターに紐づくサービス一覧を取得
	listOut, err := ecsClient.ListServices(ctx, &ecs.ListServicesInput{
		Cluster: aws.String(clusterName),
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

		// 1. DesiredCount = 0 に更新
		_, err := ecsClient.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster:      aws.String(clusterName),
			Service:      aws.String(svcName),
			DesiredCount: aws.Int32(0),
		})
		if err != nil {
			log.Printf("Failed to update service(%s) desiredCount=0: %v", svcName, err)
			continue
		}

		// 2. サービスが STABLE になるまで待機
		if err := waitForServiceStable(ctx, ecsClient, clusterName, svcName); err != nil {
			log.Printf("waitForServiceStable failed for service(%s): %v", svcName, err)
		}

		// 3. サービス削除 (Force=true)
		log.Printf("[Service: %s] Deleting...", svcName)
		_, err = ecsClient.DeleteService(ctx, &ecs.DeleteServiceInput{
			Cluster: aws.String(clusterName),
			Service: aws.String(svcName),
			Force:   aws.Bool(true),
		})
		if err != nil {
			log.Printf("Failed to delete service(%s): %v", svcName, err)
		}
	}

	return nil
}

// ============================================
// クラスターに残っているタスクを停止
// ============================================
func stopRemainingTasks(ctx context.Context, cfg aws.Config, clusterName string) error {
	ecsClient := ecs.NewFromConfig(cfg)

	listOut, err := ecsClient.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       aws.String(clusterName),
		DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return fmt.Errorf("ListTasks error: %w", err)
	}

	if len(listOut.TaskArns) == 0 {
		log.Printf("No running tasks found in cluster: %s", clusterName)
		return nil
	}

	for _, taskArn := range listOut.TaskArns {
		taskName := arnToName(taskArn)
		log.Printf("[Task: %s] Stopping...", taskName)
		_, err := ecsClient.StopTask(ctx, &ecs.StopTaskInput{
			Cluster: aws.String(clusterName),
			Task:    aws.String(taskArn),
			Reason:  aws.String("Cleanup before destroy"),
		})
		if err != nil {
			log.Printf("Failed to stop task(%s): %v", taskName, err)
		}
	}

	return nil
}

// ============================================
// cdk destroy の実行
// ============================================
func runCdkDestroy(stackName, profile, region, cdkAppDir, cdkAppFile string) error {
	// cdk destroy の引数
	args := []string{"destroy", "--all", "--force"}

	// プロファイル指定
	if profile != "" {
		args = append(args, "--profile", profile)
	}

	// 例: /path/to/cdk-app + main.ts → /path/to/cdk-app/main.ts
	appPath := filepath.Join(cdkAppDir, cdkAppFile)
	// Windows/Mac/Linux など環境を気にせず安全にパスを連結

	// --app "npx ts-node /path/to/cdk-app/main.ts"
	appArg := fmt.Sprintf("npx ts-node %s", appPath)
	args = append(args, "--app", appArg)

	// リージョン指定などが必要なら適宜追加
	// args = append(args, "--region", region)

	log.Printf("Executing: cdk %s", strings.Join(args, " "))
	cmd := exec.Command("cdk", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ============================================
// ARN の末尾からリソース名を取り出す関数
// ============================================
func arnToName(arn string) string {
	parts := strings.Split(arn, "/")
	return parts[len(parts)-1]
}

// ============================================
// ECSサービスが STABLE になるのを待つ
// ============================================
func waitForServiceStable(ctx context.Context, ecsClient *ecs.Client, clusterName, serviceName string) error {
	// ecs パッケージの NewServicesStableWaiter を使用
	svcWaiter := ecs.NewServicesStableWaiter(ecsClient)

	input := &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	}
	maxWait := 10 * time.Minute

	if err := svcWaiter.Wait(ctx, input, maxWait); err != nil {
		return err
	}
	return nil
}
