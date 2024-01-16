/*
Copyright 2024.

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
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"math/rand"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1 "github.com/icoolguy1995/database-operator/api/v1"
)

// DatabaseUserReconciler reconciles a DatabaseUser object
type DatabaseUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=db.staffbase.com,resources=databaseusers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.staffbase.com,resources=databaseusers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.staffbase.com,resources=databaseusers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the DatabaseUser object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *DatabaseUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	dbUser := &dbv1.DatabaseUser{}
	err := r.Get(ctx, req.NamespacedName, dbUser)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			log.Info("DatabaseUser resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get DatabaseUser")
		return ctrl.Result{}, err
	}

	switch dbUser.Spec.DatabaseType {
	case "MongoDB":
		log.Info("Processing MongoDB user", "User", dbUser.Spec.UserName)
		if err := r.handleMongoDBUser(ctx, dbUser); err != nil {
			log.Error(err, "Error handling MongoDB user")
			return ctrl.Result{}, err
		}
	case "Postgres":
		log.Info("Processing Postgres user", "User", dbUser.Spec.UserName)
		if err := r.handlePostgresUser(ctx, dbUser); err != nil {
			log.Error(err, "Error handling Postgres user")
			return ctrl.Result{}, err
		}
	default:
		// Log and handle unknown or unsupported database types
		log.Info("Unsupported database type", "DatabaseType", dbUser.Spec.DatabaseType)
		// Optionally, update the DatabaseUser status to reflect the unsupported database type
		// return an error or simply skip processing this request
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DatabaseUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1.DatabaseUser{}).
		Complete(r)
}

func (r *DatabaseUserReconciler) handleMongoDBUser(ctx context.Context, dbUser *dbv1.DatabaseUser) error {
	log := log.FromContext(ctx)

	// Connect to MongoDB
	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://root:password123@mongodb-0.mongodb-headless.mongo-helm.svc.cluster.local:27017,mongodb-1.mongodb-headless.mongo-helm.svc.cluster.local:27017,mongodb-2.mongodb-headless.mongo-helm.svc.cluster.local:27017/admin?replicaSet=rs0"))
	if err != nil {
		log.Error(err, "Unable to connect to MongoDB")
		return err
	}
	defer mongoClient.Disconnect(ctx)

	username := dbUser.Spec.UserName
	password := generateRandomPassword()

	database := mongoClient.Database("admin")

	createUserCmd := bson.D{
		{Key: "createUser", Value: username},
		{Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{
			bson.M{"role": "readWrite", "db": dbUser.Spec.DatabaseName},
		}},
	}

	result := database.RunCommand(ctx, createUserCmd)
	if result.Err() != nil {
		log.Error(result.Err(), "Failed to create MongoDB user")
		return result.Err()
	}

	// Create/update Kubernetes secret
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mongodb-secret-" + username,
			Namespace: dbUser.Namespace,
		},
		StringData: map[string]string{
			"username": username,
			"password": password,
		},
	}
	// Set DatabaseUser instance as the owner and controller
	if err := controllerutil.SetControllerReference(dbUser, secret, r.Scheme); err != nil {
		return err
	}
	found := &v1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, found)
	if err != nil &&
		errors.IsNotFound(err) {
		log.Info("Creating a new Secret", "Secret.Namespace", secret.Namespace, "Secret.Name", secret.Name)
		err = r.Create(ctx, secret)
		if err != nil {
			log.Error(err, "Failed to create Kubernetes Secret")
			return err
		}
	} else if err != nil {
		log.Error(err, "Failed to get Secret")
		return err
	}

	log.Info("Successfully processed MongoDB user", "User", username)
	return nil
}
func quoteLiteral(input string) string {
	// Properly escape single quotes for SQL
	return "'" + strings.ReplaceAll(input, "'", "''") + "'"
}

func (r *DatabaseUserReconciler) handlePostgresUser(ctx context.Context, dbUser *dbv1.DatabaseUser) error {
	log := log.FromContext(ctx)
	// Connect to PostgreSQL
	conn, err := pgx.Connect(ctx, "postgres://zalando:hVWcuJiHHQNUqNhC6njeLKUX1zF9Y0kVeKeagpFaTPSLKdJzLNERvuxv2QhKHOqv@acid-minimal-cluster.zalando.svc.cluster.local/postgres?sslmode=require")
	if err != nil {
		log.Error(err, "Unable to connect to PostgreSQL")
		return err
	}
	defer conn.Close(ctx)

	username := dbUser.Spec.UserName
	password := generateRandomPassword()
	databaseName := dbUser.Spec.DatabaseName

	// Create or update user
	_, err = conn.Exec(ctx, "SELECT 1 FROM pg_roles WHERE rolname = $1", username)
	if err != nil {
		log.Error(err, "Failed to query user role")
		return err
	}
	createUserSQL := fmt.Sprintf("CREATE USER %s WITH PASSWORD %s", username, quoteLiteral(password))
	_, err = conn.Exec(ctx, createUserSQL)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL user")
		return err
	}
	log.Info("Created PostgreSQL user successfully")

	createDBSQL := fmt.Sprintf("CREATE DATABASE %s", databaseName)
	_, err = conn.Exec(ctx, createDBSQL)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL database")
		return err
	}
	log.Info("Created PostgreSQL database successfully", "database", databaseName)

	// Grant read-write privileges to the specific database
	grnt := fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s to %s", databaseName, username)
	_, err = conn.Exec(ctx, grnt)
	if err != nil {
		log.Error(err, "Failed to grant privileges on the database to the user")
		return err
	}

	// Create/update Kubernetes secret
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-secret-" + username,
			Namespace: dbUser.Namespace,
		},
		StringData: map[string]string{
			"username": username,
			"password": password,
		},
	}
	// Set DatabaseUser instance as the owner and controller
	if err := controllerutil.SetControllerReference(dbUser, secret, r.Scheme); err != nil {
		return err
	}

	found := &v1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, found)
	if err != nil &&
		errors.IsNotFound(err) {
		log.Info("Creating a new Secret", "Secret.Namespace", secret.Namespace, "Secret.Name", secret.Name)
		err = r.Create(ctx, secret)
		if err != nil {
			log.Error(err, "Failed to create Kubernetes Secret")
			return err
		}
	} else if err != nil {
		log.Error(err, "Failed to get Secret")
		return err
	}

	log.Info("Successfully processed Postgres user", "User", username)
	return nil
}

func generateRandomPassword() string {
	// Example: Simple random password generation logic.
	// Replace this with a more robust password generation mechanism.
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	password := make([]rune, 10)
	for i := range password {
		password[i] = letters[rand.Intn(len(letters))]
	}
	return string(password)
}
