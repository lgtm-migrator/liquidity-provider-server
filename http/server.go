package http

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"net/http"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/rsksmart/liquidity-provider-server/http/models"

	"github.com/ethereum/go-ethereum/common"
	federation "github.com/rsksmart/liquidity-provider-server/helpers"

	"context"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/rsksmart/liquidity-provider-server/connectors"
	"github.com/rsksmart/liquidity-provider-server/storage"
	"github.com/rsksmart/liquidity-provider/providers"
	"github.com/rsksmart/liquidity-provider/types"
	log "github.com/sirupsen/logrus"
)

type Server struct {
	srv                  http.Server
	providers            []providers.LiquidityProvider
	rsk                  *connectors.RSK
	btc                  *connectors.BTC
	db                   *storage.DB
	irisActivationHeight int
	erpKeys              []string
	lbcAddr              string
}

func New(rsk *connectors.RSK, btc *connectors.BTC, db *storage.DB, irisActivationHeight int, erpKeys []string, lbcAddr string) Server {
	var liqProviders []providers.LiquidityProvider
	return Server{
		rsk:                  rsk,
		btc:                  btc,
		db:                   db,
		providers:            liqProviders,
		irisActivationHeight: irisActivationHeight,
		erpKeys:              erpKeys,
		lbcAddr:              lbcAddr,
	}
}

func (s *Server) AddProvider(lp providers.LiquidityProvider) {
	s.providers = []providers.LiquidityProvider{lp}
}

func (s *Server) Start(port uint) error {
	r := mux.NewRouter()
	r.Path("/getQuote").Methods(http.MethodPost).HandlerFunc(s.getQuoteHandler)
	r.Path("/acceptQuote").Methods(http.MethodPost).HandlerFunc(s.acceptQuoteHandler)
	w := log.StandardLogger().WriterLevel(log.DebugLevel)
	h := handlers.LoggingHandler(w, r)
	defer w.Close()

	s.srv = http.Server{
		Addr:    ":" + fmt.Sprint(port),
		Handler: h,
	}
	log.Info("starting server at localhost", s.srv.Addr)

	err := s.srv.ListenAndServe()
	if err != http.ErrServerClosed {
		return err
	}

	log.Info("started server at localhost", s.srv.Addr)
	return nil
}

func (s *Server) Shutdown() {
	log.Info("stopping server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		log.Fatal("server shutdown failed: ", err)
	}
	log.Info("server stopped")
}

func (s *Server) getQuoteHandler(w http.ResponseWriter, r *http.Request) {
	qr := models.QuoteRequest{}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&qr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Debug("received quote request: ", fmt.Sprintf("%+v", qr))

	gas, err := s.rsk.EstimateGas(qr.CallContractAddress, qr.ValueToTransfer, []byte(qr.CallContractArguments))
	if err != nil {
		log.Error("error estimating gas: ", err.Error())
		http.Error(w, "error estimating gas", http.StatusInternalServerError)
		return
	}

	price, err := s.rsk.GasPrice()
	if err != nil {
		log.Error("error estimating gas price: ", err.Error())
		http.Error(w, "error estimating gas price", http.StatusInternalServerError)
		return
	}

	var quotes []*types.Quote
	fedAddress, err := s.rsk.GetFedAddress()
	if err != nil {
		log.Error("error retrieving federation address: ", err.Error())
		http.Error(w, "error retrieving federation address", http.StatusInternalServerError)
		return
	}

	q := parseReqToQuote(qr, s.lbcAddr, fedAddress)
	for _, p := range s.providers {
		pq := p.GetQuote(q, gas, *price)
		if pq != nil {
			err = s.storeQuote(pq)

			if err != nil {
				log.Error(err)
			} else {
				quotes = append(quotes, pq)
			}
		}
	}

	enc := json.NewEncoder(w)
	err = enc.Encode(&quotes)
	if err != nil {
		log.Error("error encoding quote list: ", err.Error())
		http.Error(w, "error processing quotes", http.StatusInternalServerError)
		return
	}
}

func parseReqToQuote(qr models.QuoteRequest, lbcAddr string, fedAddr string) types.Quote {
	return types.Quote{
		LBCAddr:       lbcAddr,
		FedBTCAddr:    fedAddr,
		BTCRefundAddr: qr.BitcoinRefundAddress,
		RSKRefundAddr: qr.RskRefundAddress,
		ContractAddr:  qr.CallContractAddress,
		Data:          qr.CallContractArguments,
		Value:         qr.ValueToTransfer,
		GasLimit:      qr.GasLimit,
	}
}

func (s *Server) acceptQuoteHandler(w http.ResponseWriter, r *http.Request) {
	type acceptReq struct {
		QuoteHash string
	}

	type acceptRes struct {
		Signature                 string `json:"signature"`
		BitcoinDepositAddressHash string `json:"bitcoinDepositAddressHash"`
	}
	req := acceptReq{}
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := acceptRes{}

	hashBytes, err := hex.DecodeString(req.QuoteHash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	quote, err := s.db.GetQuote(req.QuoteHash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	btcRefAddr, err := federation.GetBytesFromBtcAddress(quote.BTCRefundAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lpBTCAddr, err := federation.GetBytesFromBtcAddress(quote.LPBTCAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lbcAddr, err := getLbcAddressBytes(quote)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	derivationValue, err := federation.GetDerivationValueHash(
		btcRefAddr, lbcAddr, lpBTCAddr, hashBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	signature, err := s.getSignatureFromHash(req.QuoteHash, hashBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response.Signature = signature

	fedSize, err := s.rsk.GetFedSize()
	if err != nil {
		log.Error("error fetching federation size: ", err.Error())
		http.Error(w, "there was an error retrieving the fed size.", http.StatusInternalServerError)
		return
	}

	var pubKeys []string
	for i := 0; i < fedSize; i++ {
		pubKey, err := s.rsk.GetFedPublicKey(i)
		if err != nil {
			log.Error("error fetching fed public key: ", err.Error())
			http.Error(w, "there was an error retrieving public key from fed.", http.StatusInternalServerError)
			return
		}

		pubKeys = append(pubKeys, pubKey)
	}

	fedThreshold, err := s.rsk.GetFedThreshold()
	if err != nil {
		log.Error("error fetching federation size: ", err.Error())
		http.Error(w, "there was an error retrieving the fed threshold.", http.StatusInternalServerError)
		return
	}

	fedAddress, err := s.getFedAddress()
	if err != nil {
		log.Error("error fetching federation address: ", err.Error())
		http.Error(w, "there was an error retrieving the fed address.", http.StatusInternalServerError)
		return
	}
	activeFedBlockHeight, err := s.rsk.GetActiveFederationCreationBlockHeight()
	if err != nil {
		log.Error("error fetching federation address: ", err.Error())
		http.Error(w, "there was an error retrieving the fed address.", http.StatusInternalServerError)
		return
	}

	fedInfo := &federation.FedInfo{
		FedThreshold:         fedThreshold,
		FedSize:              fedSize,
		PubKeys:              pubKeys,
		FedAddress:           fedAddress.ScriptAddress(),
		ActiveFedBlockHeight: activeFedBlockHeight,
		IrisActivationHeight: s.irisActivationHeight,
		ErpKeys:              s.erpKeys,
	}

	params := s.btc.GetParams()
	derivedFedAddress, err := federation.GetDerivedBitcoinAddressHash(derivationValue, fedInfo, &params)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response.BitcoinDepositAddressHash = derivedFedAddress.EncodeAddress()

	enc := json.NewEncoder(w)
	err = enc.Encode(response)

	// TODO: ensure that the quote is not processed if there is any kind of error in the communication with the client
	if err != nil {
		log.Error("error encoding response: ", err.Error())
		http.Error(w, "error processing request", http.StatusInternalServerError)
		return
	}
}

func (s *Server) getFedAddress() (btcutil.Address, error) {
	fedAddressStr, err := s.rsk.GetFedAddress()
	if err != nil {
		return nil, err
	}
	params := s.btc.GetParams()
	fedAddress, err := btcutil.DecodeAddress(fedAddressStr, &params)
	if err != nil {
		return nil, err
	}
	return fedAddress, nil
}

func (s *Server) getSignatureFromHash(hash string, hashBytes []byte) (string, error) {

	quote, err := s.db.GetQuote(hash)
	if err != nil {
		return "", err
	}
	if quote == nil {
		return "", fmt.Errorf("quote not found : %v", hash)
	}
	p := getProviderByAddress(s.providers, quote.LPRSKAddr)

	signature, err := p.SignHash(hashBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(signature), nil
}

func getProviderByAddress(liquidityProviders []providers.LiquidityProvider, addr string) (ret providers.LiquidityProvider) {
	for _, p := range liquidityProviders {
		if p.Address() == addr {
			return p
		}
	}
	return nil
}

func getLbcAddressBytes(quote *types.Quote) ([]byte, error) {
	if !common.IsHexAddress(quote.LBCAddr) {
		return nil, fmt.Errorf("invalid LBC Address. value: %v", quote.LBCAddr)
	}
	lbcAddr := common.FromHex(quote.LBCAddr)

	return lbcAddr, nil
}

func (s *Server) storeQuote(q *types.Quote) error {
	h, err := s.rsk.HashQuote(q)
	if err != nil {
		return fmt.Errorf("error hashing quote: %v", err)
	}

	err = s.db.InsertQuote(h, q)
	if err != nil {
		log.Fatalf("error inserting quote: %v", err)
	}
	return nil
}
