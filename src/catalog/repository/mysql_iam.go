package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	_ "embed"
	"fmt"
	"net"
	"time"

	"github.com/aws-containers/retail-store-sample-app/catalog/config"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// rdsCABundle is the public Amazon RDS certificate bundle, used to verify the
// database's TLS certificate. Source:
// https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
//
//go:embed rds-global-bundle.pem
var rdsCABundle []byte

// openMySQLWithIAM opens a MySQL connection that logs in with a short-lived
// RDS IAM auth token instead of a stored password. A fresh token (valid for
// 15 minutes) is generated for every new connection, using the AWS
// credentials of the pod (EKS Pod Identity). IAM auth requires TLS.
func openMySQLWithIAM(cfg config.DatabaseConfiguration) (*gorm.DB, error) {
	ctx := context.Background()

	endpoint := cfg.Endpoint
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		// No port given: use the MySQL default
		host = endpoint
		endpoint = net.JoinHostPort(endpoint, "3306")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, fmt.Errorf("AWS region is not set (set AWS_REGION)")
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(rdsCABundle) {
		return nil, fmt.Errorf("failed to load RDS CA bundle")
	}

	dbCfg := mysqldriver.NewConfig()
	dbCfg.User = cfg.User
	dbCfg.Net = "tcp"
	dbCfg.Addr = endpoint
	dbCfg.DBName = cfg.Name
	dbCfg.Timeout = time.Duration(cfg.ConnectTimeout) * time.Second
	dbCfg.ParseTime = true
	dbCfg.Loc = time.Local
	dbCfg.Params = map[string]string{"charset": "utf8mb4"}
	dbCfg.AllowCleartextPasswords = true // required for IAM auth tokens (sent over TLS only)
	dbCfg.TLS = &tls.Config{
		RootCAs:    rootCAs,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}

	// Generate a new token right before each new connection is made
	err = dbCfg.Apply(mysqldriver.BeforeConnect(func(ctx context.Context, c *mysqldriver.Config) error {
		token, err := auth.BuildAuthToken(ctx, endpoint, awsCfg.Region, cfg.User, awsCfg.Credentials)
		if err != nil {
			return fmt.Errorf("failed to build RDS IAM auth token: %w", err)
		}
		c.Passwd = token
		return nil
	}))
	if err != nil {
		return nil, err
	}

	connector, err := mysqldriver.NewConnector(dbCfg)
	if err != nil {
		return nil, err
	}

	sqlDB := sql.OpenDB(connector)
	// Recycle connections periodically so they don't outlive their credentials for long
	sqlDB.SetConnMaxLifetime(10 * time.Minute)

	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}
