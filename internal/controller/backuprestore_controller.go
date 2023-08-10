/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/juju/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backuprestorev1alpha1 "backuprestore.geoffrey.io/backuprestore/api/v1alpha1"
)

// BackupRestoreReconciler reconciles a BackupRestore object
type BackupRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=batch.backuprestore.geoffrey.io,resources=backuprestores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.backuprestore.geoffrey.io,resources=backuprestores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.backuprestore.geoffrey.io,resources=backuprestores/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BackupRestore object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *BackupRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("starting reconciliation")

	// Fetch the BackupRestore CR
	backupRestore := &backuprestorev1alpha1.BackupRestore{}
	err := r.Get(ctx, req.NamespacedName, backupRestore)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, probably deleted
			return reconcile.Result{}, nil
		}
		// Error fetching the CR
		return reconcile.Result{}, err
	}

	// Handle backup or restore logic based on the CR's spec
	if backupRestore.Spec.BackupSchedule != "" {
		// Perform backup logic
		err := performBackup(backupRestore)
		if err != nil {
			log.Error(err, "Backup failed")
			return reconcile.Result{}, err
		}
	} else {
		// Perform restore logic
		err := performRestore(backupRestore)
		if err != nil {
			log.Error(err, "Restore failed")
			return reconcile.Result{}, err
		}
	}

	// Update the status with the last backup/restore time
	backupRestore.Status.LastBackupTime = metav1.Now()
	err = r.Status().Update(ctx, backupRestore)
	if err != nil {
		log.Error(err, "Failed to update status")
		return reconcile.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backuprestorev1alpha1.BackupRestore{}).
		Complete(r)
}

func performBackup(backupRestore *backuprestorev1alpha1.BackupRestore) error {

	// Get the backup schedule in days
	backupScheduleDays := parseBackupSchedule(backupRestore.Spec.BackupSchedule)
	if backupScheduleDays <= 0 {
		return fmt.Errorf("invalid backup schedule: %s", backupRestore.Spec.BackupSchedule)
	}

	// Check if it's time to perform the backup
	lastBackupTime := backupRestore.Status.LastBackupTime.Time
	if time.Since(lastBackupTime).Hours() < float64(24*backupScheduleDays) {
		fmt.Printf("Not yet time for backup. Last backup: %s\n", lastBackupTime)
		return nil
	}

	// AWS S3 credentials and configuration
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String("us-east-1"), // Update with your desired region
	}))
	svc := s3.New(sess)

	// Generate a unique backup filename
	backupFileName := fmt.Sprintf("backup_%s.sql", time.Now().Format("20060102150405"))
	backupBucketName := "your-backup-bucket" // Update with your actual S3 bucket name

	// Execute a command in the sourcePod to create a backup file
	sourcePod := backupRestore.Spec.SourcePod

	cmd := exec.Command("kubectl", "exec", sourcePod, "--", "sh", "-c", "mysqldump -u user -pPASSWORD dbname > /tmp/"+backupFileName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute backup command: %v, stderr: %s", err, stderr.String())
	}

	// Upload the backup file to S3
	backupFilePath := "/tmp/" + backupFileName
	backupFile, err := os.Open(backupFilePath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %v", err)
	}
	defer backupFile.Close()

	_, err = svc.PutObject(&s3.PutObjectInput{
		Body:   backupFile,
		Bucket: aws.String(backupBucketName),
		Key:    aws.String(backupFileName),
	})
	if err != nil {
		return fmt.Errorf("failed to upload backup to S3: %v", err)
	}

	fmt.Printf("Backup saved to S3: %s/%s\n", backupBucketName, backupFileName)

	return nil
}

func performRestore(backupRestore *backuprestorev1alpha1.BackupRestore) error {
	// AWS S3 credentials and configuration
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String("us-east-1"), // Update with your desired region
	}))
	svc := s3.New(sess)

	// Download and restore the latest backup file
	backupBucketName := "your-backup-bucket" // Update with your actual S3 bucket name

	resp, err := svc.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(backupBucketName),
	})
	if err != nil {
		return err
	}

	var latestBackupKey string
	var latestBackupTime time.Time

	for _, item := range resp.Contents {
		backupTime := *item.LastModified
		if backupTime.After(latestBackupTime) {
			latestBackupTime = backupTime
			latestBackupKey = *item.Key
		}
	}

	if latestBackupKey == "" {
		return fmt.Errorf("no backup found in S3 bucket")
	}

	getObjectOutput, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(backupBucketName),
		Key:    aws.String(latestBackupKey),
	})

	if err != nil {
		return err
	}

	// Restore the backup content to the targetPod
	targetPod := backupRestore.Spec.TargetPod

	restoreErr := restoreDatabase(getObjectOutput.Body, targetPod)

	if restoreErr != nil {
		return fmt.Errorf("failed to restore database: %v", restoreErr)
	}

	fmt.Printf("Restored backup from S3: %s/%s to Pod: %s\n", backupBucketName, latestBackupKey, targetPod)
	return nil
}

func restoreDatabase(backupContent io.ReadCloser, targetPod string) error {
	// Step 1: Connect to the targetPod's MySQL database
	db, err := sql.Open("mysql", "user=root password=your-password dbname=your-database host="+targetPod+" port=3306")
	if err != nil {
		return err
	}
	defer db.Close()

	// Step 2: Create a temporary backup file on the local machine
	backupFilePath := "/tmp/backup.sql"
	backupFile, err := os.Create(backupFilePath)
	if err != nil {
		return err
	}
	defer backupFile.Close()

	// Step 3: Write the backup content to the temporary file
	_, err = io.Copy(backupFile, backupContent)
	if err != nil {
		return err
	}

	// Step 4: Execute the database restore command on the targetPod
	restoreCmd := exec.Command("kubectl", "cp", backupFilePath, targetPod+":/tmp/backup.sql")
	restoreCmd.Stdout = os.Stdout
	restoreCmd.Stderr = os.Stderr

	err = restoreCmd.Run()
	if err != nil {
		return err
	}

	// Step 5: Connect to the targetPod's MySQL database again
	db, err = sql.Open("mysql", "user=root password=your-password dbname=your-database host="+targetPod+" port=3306")
	if err != nil {
		return err
	}
	defer db.Close()

	// Step 6: Execute the database restore command on the targetPod
	_, err = db.Exec("source /tmp/backup.sql")
	if err != nil {
		return err
	}

	fmt.Printf("Database restored on Pod: %s\n", targetPod)
	return nil
}

func parseBackupSchedule(schedule string) int {
	// Parse the backup schedule in days
	// Example schedules: "daily", "weekly", "monthly"
	switch schedule {
	case "daily":
		return 1
	case "weekly":
		return 7
	case "monthly":
		return 30
	default:
		return -1
	}
}
