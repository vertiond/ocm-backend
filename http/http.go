package http

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/gertjaap/ocm-backend/logging"
	"github.com/gertjaap/ocm-backend/processor"
	"github.com/gorilla/mux"
	"github.com/paulbellamy/ratecounter"
)

type HttpServer struct {
	srv           *http.Server
	db            *sql.DB
	proc          *processor.Processor
	responseTimes map[string]*ratecounter.AvgRateCounter
}

func NewHttpServer(db *sql.DB, p *processor.Processor) *HttpServer {
	h := new(HttpServer)

	r := mux.NewRouter()
	r.HandleFunc("/info", h.infoHandler)
	r.HandleFunc("/health", h.healthHandler)
	r.HandleFunc("/balance/{script}", h.balanceHandler)
	r.HandleFunc("/utxos/{script}", h.utxosHandler)

	h.srv = &http.Server{
		Handler: r,
		Addr:    ":8000",
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	h.db = db
	h.proc = p
	h.responseTimes = map[string]*ratecounter.AvgRateCounter{
		"info":    ratecounter.NewAvgRateCounter(15 * time.Minute),
		"health":  ratecounter.NewAvgRateCounter(15 * time.Minute),
		"utxos":   ratecounter.NewAvgRateCounter(15 * time.Minute),
		"balance": ratecounter.NewAvgRateCounter(15 * time.Minute),
	}

	return h
}

func (h *HttpServer) Run() error {
	return h.srv.ListenAndServe()
}

func (h *HttpServer) infoHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	writeJson(w, map[string]interface{}{
		"tipHeight":        h.proc.TipHeight,
		"backendTipHeight": h.proc.BackendTipHeight,
		"difficulty":       h.proc.Difficulty,
	})
	h.responseTimes["info"].Incr(time.Since(start).Nanoseconds())
}

func (h *HttpServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reply := map[string]interface{}{}

	for k, v := range h.responseTimes {
		reply[fmt.Sprintf("response_time_%s", k)] = float64(v.Rate()) / float64(math.Pow(10, 6))
		reply[fmt.Sprintf("rps_last_15m_%s", k)] = float64(v.Hits()) / float64(15*60)
	}

	writeJson(w, reply)
	h.responseTimes["health"].Incr(time.Since(start).Nanoseconds())
}

func (h *HttpServer) balanceHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	vars := mux.Vars(r)

	script, err := hex.DecodeString(vars["script"])
	if err != nil {
		logging.Errorf("Error decoding script: %v", err)
		http.Error(w, "Invalid request", 500)
	}

	var scriptID int64
	scriptID = -1
	err = h.db.QueryRow("select id from scripts where script=$1", script).Scan(&scriptID)
	if err != nil {
		if err != sql.ErrNoRows {
			logging.Errorf("Error querying script: %v", err)
			http.Error(w, "Internal server error", 500)
		}
	}

	var confirmed int64
	var immature int64

	if scriptID != -1 {
		err = h.db.QueryRow("select coalesce(sum(value),0) from outputs o left join transactions t on t.id=o.created_in_tx left join blocks b on b.id=t.block_id where script_id=$1 AND (coinbase=true and b.height > (select height-101 from blocks order by height desc limit 1)) AND spent_in_tx IS NULL", scriptID).Scan(&immature)
		if err != nil {
			logging.Errorf("Error querying immature balance: %v", err)
			http.Error(w, "Internal server error", 500)
		}
		err = h.db.QueryRow("select coalesce(sum(value),0) from outputs o left join transactions t on t.id=o.created_in_tx left join blocks b on b.id=t.block_id where script_id=$1 AND (coinbase=false or b.height <= (select height-101 from blocks order by height desc limit 1)) AND spent_in_tx IS NULL", scriptID).Scan(&confirmed)
		if err != nil {
			logging.Errorf("Error querying confirmed balance: %v", err)
			http.Error(w, "Internal server error", 500)
		}
	}

	writeJson(w, map[string]interface{}{
		"confirmed": confirmed,
		"maturing":  immature,
	})
	h.responseTimes["balance"].Incr(time.Since(start).Nanoseconds())
}

type Utxo struct {
	TxID   string `json:"txid"`
	Vout   int64  `json:"vout"`
	Amount int64  `json:"satoshis"`
}

func (h *HttpServer) utxosHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	vars := mux.Vars(r)

	script, err := hex.DecodeString(vars["script"])
	if err != nil {
		logging.Errorf("Error decoding script: %v", err)
		http.Error(w, "Invalid request", 500)
	}

	var scriptID int64
	scriptID = -1
	err = h.db.QueryRow("select id from scripts where script=$1", script).Scan(&scriptID)
	if err != nil {
		if err != sql.ErrNoRows {
			logging.Errorf("Error querying script: %v", err)
			http.Error(w, "Internal server error", 500)
		}
	}

	result := make([]Utxo, 0)
	if scriptID != -1 {
		rows, err := h.db.Query("select t.hash, o.vout, o.value from outputs o left join transactions t on t.id=o.created_in_tx left join blocks b on b.id=t.block_id where script_id=$1 AND (coinbase=false or b.height <= (select height-101 from blocks order by height desc limit 1))", scriptID)
		if err != nil {
			logging.Errorf("Error querying utxos: %v", err)
			http.Error(w, "Internal server error", 500)
		}
		for rows.Next() {
			var txid []byte
			var vout int64
			var value int64
			err = rows.Scan(&txid, &vout, &value)
			if err == nil {
				h, err := chainhash.NewHash(txid)
				if err == nil {
					result = append(result, Utxo{
						Vout:   vout,
						Amount: value,
						TxID:   h.String(),
					})
				} else {
					logging.Warnf("Utxo has invalid tx hash: %v", err)
				}
			} else {
				logging.Warnf("Error scanning utxo row: %v", err)
			}
		}
	}

	writeJson(w, result)
	h.responseTimes["utxos"].Incr(time.Since(start).Nanoseconds())
}

func writeJson(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(v)
}