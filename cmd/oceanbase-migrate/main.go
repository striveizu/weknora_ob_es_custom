// oceanbase-migrate initializes a NEW OceanBase MySQL-mode database using the
// embedded PoC schema. It never auto-recovers partially applied DDL.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/Tencent/WeKnora/internal/database"
)

func main() {
	for _, name := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_NAME"} {
		if os.Getenv(name) == "" {
			log.Fatalf("%s is required", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := database.RunOceanBaseMigrations(ctx, database.MySQLDSN()); err != nil {
		log.Fatal(err)
	}
	log.Print("OceanBase PoC schema ready")
}
