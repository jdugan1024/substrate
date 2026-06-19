package brain

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminPoolPrefersDedicatedPool(t *testing.T) {
	mainPool := &pgxpool.Pool{}
	adminPool := &pgxpool.Pool{}
	app := &App{Pool: mainPool, AdminPool: adminPool}

	if got := app.adminPool(); got != adminPool {
		t.Fatalf("adminPool() = %p, want dedicated admin pool %p", got, adminPool)
	}
}

func TestAdminPoolFallsBackToMainPool(t *testing.T) {
	mainPool := &pgxpool.Pool{}
	app := &App{Pool: mainPool}

	if got := app.adminPool(); got != mainPool {
		t.Fatalf("adminPool() = %p, want main pool %p when no admin pool configured", got, mainPool)
	}
}
