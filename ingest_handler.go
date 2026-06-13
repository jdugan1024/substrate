// ABOUTME: HTTP handler for POST /ingest — accepts a conversation transcript batch.
// ABOUTME: Authenticated via authMiddleware (PAT or OIDC); delegates to IngestService.

package main

import (
	"encoding/json"
	"log"
	"net/http"

	"open-brain-go/brain/service"
)

func ingestHandler(ingest *service.IngestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var batch service.IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		if batch.Tool == "" || batch.SessionID == "" {
			http.Error(w, `{"error":"tool and session_id are required"}`, http.StatusBadRequest)
			return
		}

		result, err := ingest.Ingest(r.Context(), batch)
		if err != nil {
			log.Printf("ingest error (tool=%s session=%s): %v", batch.Tool, batch.SessionID, err)
			http.Error(w, `{"error":"ingest failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
