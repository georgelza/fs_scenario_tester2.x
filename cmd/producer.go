/*****************************************************************************
*
*	File			: producer.go
*
* 	Created			: 27 Aug 2021
*
*	Description		: Creates Fake JSON Payload, originally posted onto Kafka topic, modified to post onto FeatureSpace event API endpoint
*
*	Modified		: 27 Aug 2021	- Start
*					: 24 Jan 2023   - Mod for applab sandbox
*					: 20 Feb 2023	- repackaged for TFM 2.0 load generation, we're creating fake FS Payment events messages onto
*					:				- FeatureSpace API endpoint
*
*					: 09 May 2023	- Enhancement: we can now read a directory of files (JSON structured), each as a paymentEvent and
*					:				- post the contents onto the API endpoint, allowing for pre designed schenarios to be used/tested
*					:				- NOTE, Original idea/usage was posting payloads onto Kafka topic, thus the fake data gen,
*					:				- With the new usage of reading scenario files allot of the bits below can be removed...
*					:				- Also removed the Prometheus instrumentation form this version as it will mostly be used to input/post a
*					: 				- coupld of files, not a big batch that needs to be timed/measure.
*
*					: 12 May 2023 	- Moved all environment variables from .exps environment export file to *_app.json file, this works better with a App
*									- destined for a desktop vs a app for a K8S server which prefers environment vars.
*					:				- https://onexlab-io.medium.com/golang-config-file-best-practise-d27d6a97a65a
*
*					: 24 May 2023	- Moved the seed data to a json structure seed.json thats ready in and then utilised instead of the seed package
*					:				- Modifying the payment structure to be aligned with Kiveshan's excell spread sheet, Makes for better down the line
*					:				- Fake data generation.
*					:				- also introduced the min and max transaction values.
*					:				- This required that I split the paymentNRT andpaymentRT into 2 dif functions as they are VERY different.
*
*					: 15 Jun 2023	- Expanded the tenant type to include BranchRangeStart/BranchRangeEnd and Bicfi, updated seed file, using
*					:				- possible brang number for toFIBrance and fromFIBrance
*
*					: 18 Jun 2023	- refactured so that we first pick a random payer and payerr account and then use the
*					:				- acocunts TenantId to fetch the tenant related information.
*					:				- staggered/split the tenants into RT and NRT as sub section under Tenant.
*					:				- staggered/split the accounts into Good: [] and Bad: [] as sub sections under Accounts
*					:				- Added additonal fields as determined through schema workshops to TenantRT message structure
*					:				- Removed Good and BadEntities
*					:
*					: 21 Aug 2023	- Change the fake data generation to be financial transaction based. if RT mode then it will generate a RT and NRT
*					: (2.0)			- if non RT then it creates 2 x NRT based payments.
*					:				- Add capability on the file input functionality to read/post AddProxy files.
*
*					: 28 Sept 2023	- For processing from file, collapsing inbound and outbound event into a single file (2 json payloads inside a array),
*					: 				- to enable us to refresh the trasanctionid so that both events match.
*
*					: 4 Oct 2023	- Adding Prometheus metrics, via a Prometheus push gateway architecture.
*
*
*
*	By				: George Leonard (georgelza@gmail.com)
*
*	jsonformatter 	: https://jsonformatter.curiousconcept.com/#
*
*****************************************************************************/

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/brianvoe/gofakeit"
	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/TylerBrock/colorjson"
	"github.com/tkanos/gonfig"
	glog "google.golang.org/grpc/grpclog"

	// My Types/Structs/functions
	"cmd/types"

	"crypto/tls"
	"crypto/x509"
	// Filter JSON array
)

type Metrics struct {
	api_pmnt_info          *prometheus.GaugeVec     // Records/Files/Transactions to process
	api_pmnt_duration      *prometheus.HistogramVec // API Call time per event
	err_pmnt_processed     *prometheus.CounterVec   // transactions/events ending in neither http 200 or 204
	api_addpayee_duration  *prometheus.HistogramVec // API Call time per event
	err_addpayee_processed *prometheus.CounterVec   // transactions/events ending in neither http 200 or 204
}

var (
	grpcLog glog.LoggerV2
	//validate = validator.New()
	varSeed  types.TPSeed
	vGeneral types.Tp_general
	pathSep  = string(os.PathSeparator)

	// We use a registry here to benefit from the consistency checks that
	// happen during registration.
	reg    = prometheus.NewRegistry()
	m      = NewMetrics(reg)
	pusher *push.Pusher
)

func init() {

	// Keeping it very simple
	grpcLog = glog.NewLoggerV2(os.Stdout, os.Stdout, os.Stdout)

	grpcLog.Infoln("###############################################################")
	grpcLog.Infoln("#")
	grpcLog.Infoln("#   Project   : TFM 2")
	grpcLog.Infoln("#")
	grpcLog.Infoln("#   Comment   : FeatureSpace Scenario Publisher / Fake Data Generator")
	grpcLog.Infoln("#             : To be Event/Alert publisher")
	grpcLog.Infoln("#")
	grpcLog.Infoln("#   By        : George Leonard (georgelza@gmail.com)")
	grpcLog.Infoln("#")
	grpcLog.Infoln("#   Date/Time :", time.Now().Format("2006-01-02 15:04:05"))
	grpcLog.Infoln("#")
	grpcLog.Infoln("###############################################################")
	grpcLog.Infoln("")
	grpcLog.Infoln("")

}

func NewMetrics(reg prometheus.Registerer) *Metrics {

	m := &Metrics{

		// How many records/files/transactions are we going to push labelled by service name
		api_pmnt_info: prometheus.NewGaugeVec(prometheus.GaugeOpts{ // Shows value, can go up and down
			Name: "txn_count",
			Help: "The number of records discovered to be processed for FS batch",
		}, []string{"hostname", "service"}),

		// PS msg_type is the eventType (either paymentNRT or paymentRT)
		// PS participant is the tenantId
		api_pmnt_duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{ // used to store timed values, includes a count
			Name: "fs_api_pmnt_duration_seconds",
			Help: "Duration of the FS API requests in seconds",
			// 4 times larger apdex status
			// Buckets: prometheus.ExponentialBuckets(0.1, 1.5, 5),
			// Buckets: prometheus.LinearBuckets(0.1, 5, 15),
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 100},
		}, []string{"hostname", "msg_type", "service", "participant", "direction", "payment_method", "score"}),

		err_pmnt_processed: prometheus.NewCounterVec(prometheus.CounterOpts{ // can only go up/increment, but usefull combined with rate, resets to zero at restart.
			Name: "fs_err_pmnt_processed_total",
			Help: "The number of err transactions processed for the FS.",
		}, []string{"hostname", "msg_type", "service", "participant", "direction", "payment_method"}),

		api_addpayee_duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{ // used to store timed values, includes a count
			Name: "fs_api_addpayee_duration_seconds",
			Help: "Duration of the FS API requests in seconds",
			// 4 times larger apdex status
			// Buckets: prometheus.ExponentialBuckets(0.1, 1.5, 5),
			// Buckets: prometheus.LinearBuckets(0.1, 5, 15),
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 100},
		}, []string{"hostname", "msg_type", "service", "participant", "direction", "score"}),

		err_addpayee_processed: prometheus.NewCounterVec(prometheus.CounterOpts{ // can only go up/increment, but usefull combined with rate, resets to zero at restart.
			Name: "fs_err_addpayee_processed_total",
			Help: "The number of err transactions processed for the FS.",
		}, []string{"hostname", "msg_type", "service", "participant", "direction"}),
	}

	reg.MustRegister(m.api_pmnt_info, m.api_pmnt_duration, m.err_pmnt_processed, m.api_addpayee_duration, m.err_addpayee_processed)

	return m
}

func loadConfig(params ...string) types.Tp_general {

	var err error

	vGeneral := types.Tp_general{}
	env := "dev"
	if len(params) > 0 { // Input environment was specified, so lets use it
		env = params[0]
		grpcLog.Info("*")
		grpcLog.Info("* Called with Argument => ", env)
		grpcLog.Info("*")

	}

	vGeneral.CurrentPath, err = os.Getwd()
	if err != nil {
		grpcLog.Fatalln("Problem retrieving current path: %s", err)

	}

	vGeneral.OSName = runtime.GOOS

	fileName := fmt.Sprintf("%s%s%s_app.json", vGeneral.CurrentPath, pathSep, env)
	err = gonfig.GetConf(fileName, &vGeneral)
	if err != nil {
		grpcLog.Fatalln("Error Reading Config File: ", err)

	} else {

		vHostname, err := os.Hostname()
		if err != nil {
			grpcLog.Fatalln("Can't retrieve hostname %s", err)

		}
		vGeneral.Hostname = vHostname

		vGeneral.Cert_file = fmt.Sprintf("%s%s%s%s%s", vGeneral.CurrentPath, pathSep, vGeneral.Cert_dir, pathSep, vGeneral.Cert_file)
		vGeneral.Cert_key = fmt.Sprintf("%s%s%s%s%s", vGeneral.CurrentPath, pathSep, vGeneral.Cert_dir, pathSep, vGeneral.Cert_key)

		vGeneral.Output_path = fmt.Sprintf("%s%s%s", vGeneral.CurrentPath, pathSep, vGeneral.Output_path)

		if vGeneral.Json_from_file == 1 {
			vGeneral.Input_path = fmt.Sprintf("%s%s%s", vGeneral.CurrentPath, pathSep, vGeneral.Input_path)

		} else {
			vGeneral.Input_path = ""

		}

		vGeneral.SeedFile = fmt.Sprintf("%s%s%s", vGeneral.CurrentPath, pathSep, vGeneral.SeedFile)

	}

	if vGeneral.EchoConfig == 1 {
		printConfig(vGeneral)
	}

	if vGeneral.Debuglevel > 0 {
		grpcLog.Infoln("*")
		grpcLog.Infoln("* Config:")
		grpcLog.Infoln("* Current path:", vGeneral.CurrentPath)
		grpcLog.Infoln("* Config File :", fileName)
		grpcLog.Infoln("*")

	}

	return vGeneral
}

func loadSeed(fileName string) types.TPSeed {

	var vSeed types.TPSeed

	err := gonfig.GetConf(fileName, &vSeed)
	if err != nil {
		grpcLog.Fatalln("Error Reading Seed File: ", err)

	}

	v, err := json.Marshal(vSeed)
	if err != nil {
		grpcLog.Fatalln("Marchalling error: ", err)
	}

	if vGeneral.EchoSeed == 1 {
		prettyJSON(string(v))

	}

	if vGeneral.Debuglevel > 0 {
		grpcLog.Infoln("*")
		grpcLog.Infoln("* Seed :")
		grpcLog.Infoln("* Current path:", vGeneral.CurrentPath)
		grpcLog.Infoln("* Seed File :", vGeneral.SeedFile)
		grpcLog.Infoln("*")

	}

	return vSeed
}

func printConfig(vGeneral types.Tp_general) {

	grpcLog.Info("****** General Parameters *****")
	grpcLog.Info("*")
	grpcLog.Info("* Hostname is\t\t\t", vGeneral.Hostname)
	grpcLog.Info("* OS is \t\t\t", vGeneral.OSName)
	grpcLog.Info("*")
	grpcLog.Info("* Debug Level is\t\t", vGeneral.Debuglevel)
	grpcLog.Info("*")
	grpcLog.Info("* Sleep Duration is\t\t", vGeneral.Sleep)
	grpcLog.Info("* Test Batch Size is\t\t", vGeneral.Testsize)
	grpcLog.Info("* Seed File is\t\t\t", vGeneral.SeedFile)
	grpcLog.Info("* Echo Seed is\t\t", vGeneral.EchoSeed)
	grpcLog.Info("* Echo JSON is\t\t", vGeneral.Echojson)
	grpcLog.Info("*")

	grpcLog.Info("* Current Path is \t\t", vGeneral.CurrentPath)
	grpcLog.Info("* Cert file is\t\t", vGeneral.Cert_file)
	grpcLog.Info("* Cert key is\t\t\t", vGeneral.Cert_key)

	grpcLog.Info("* HTTP JSON POST URL is\t", vGeneral.Httpposturl)

	grpcLog.Info("* Read JSON from file is\t", vGeneral.Json_from_file) // if 0 then we create fake data else
	grpcLog.Info("* Input path is\t\t", vGeneral.Input_path)            // if 1 then read files from input_path
	grpcLog.Info("* Data Gen Mode is\t\t", vGeneral.Datamode)           // if we're creating fake data then who's the input system
	grpcLog.Info("* Source Sys is\t\t", vGeneral.Sourcesystem)          // This defines which Source system we generating as

	grpcLog.Info("* Output JSON to file is\t", vGeneral.Json_to_file)
	grpcLog.Info("* Output path is\t\t", vGeneral.Output_path)

	grpcLog.Info("* MinTransactionValue is\tR ", vGeneral.MinTransactionValue)
	grpcLog.Info("* MaxTransactionValue is\tR ", vGeneral.MaxTransactionValue)

	grpcLog.Info("*")
	grpcLog.Info("*******************************")

	grpcLog.Info("")

}

// Pretty Print JSON string
func prettyJSON(ms string) {

	var obj map[string]interface{}

	json.Unmarshal([]byte(ms), &obj)

	// Make a custom formatter with indent set
	f := colorjson.NewFormatter()
	f.Indent = 4

	// Marshall the Colorized JSON
	result, _ := f.Marshal(obj)
	fmt.Println(string(result))

}

// We're driving this from an account perspective. so lets find the tenants information as per the account def
// Helper Func - Find the tenandId for the Bank sending or receiving the funds.
func findTenant(tenants []types.TTenant, filter string) (ret types.TTenant, err error) {

	for _, item := range tenants {
		if item.TenantId == filter {
			ret = item
			return ret, nil
		}
	}
	err = errors.New("tenant not found")

	return ret, err
}

// - FAKE Data generation
// - Build a fin transaction.
// 1. are we doing "hist" or rpp, if hist then we also use sourcesystem to control which source system is used.
// the datamode and source system is used together during the prometheus logging
// 2. Step 1 select debitor
// 3. Step 2 find creditor
// 4. build outbound record
// 5. build inbound record
// 6. send
// 7. print to file
// 8. print to screen

func constructFakeFinTransaction() (t_OutboundPayment map[string]interface{}, t_InboundPayment map[string]interface{}, err error) {

	var paymentStream string
	var TransactionTypeNRT string
	var localInstrument string
	var jDebtorAccount types.TAccount
	var jCreditorAccount types.TAccount
	var jDebtorBank types.TTenant
	var jCreditorBank types.TTenant
	var DebtorFIBranchId string
	var CreditorFIBranchId string

	var txnId string
	var eventTime string
	var requestExecutionDate string
	var settlementDate string
	var paymentClearingSystemReference string
	var paymentRef string
	var remittanceId string
	var msgType string

	// We just using gofakeit to pad the json document size a bit.
	//
	// https://github.com/brianvoe/gofakeit
	// https://pkg.go.dev/github.com/brianvoe/gofakeit

	gofakeit.Seed(0)

	nAmount := gofakeit.Price(vGeneral.MinTransactionValue, vGeneral.MaxTransactionValue)
	t_amount := &types.TAmount{
		BaseCurrency: "zar",
		BaseValue:    nAmount,
		Currency:     "zar",
		Value:        nAmount,
	}

	// Determine how many accounts we have in seed file, it's all about them...
	// and build the 2 structures from that viewpoint
	accountsCount := len(varSeed.Accounts.Good) - 1
	nDebtorAccount := gofakeit.Number(0, accountsCount)
	nCreditorAccount := gofakeit.Number(0, accountsCount)

	// check to make sure the 2 are not the same, if they are, redo...
	if nDebtorAccount == nCreditorAccount {
		nCreditorAccount = gofakeit.Number(0, accountsCount)
	}

	// find the debtor and creditor record to use
	jDebtorAccount = varSeed.Accounts.Good[nDebtorAccount]
	jCreditorAccount = varSeed.Accounts.Good[nCreditorAccount]

	if vGeneral.Datamode == "hist" {

		/*
			// select a random paymentStream, from the historical options
			paymentStreamCount := len(varSeed.PaymentStreamNRT) - 1
			nPaymentStream := gofakeit.Number(0, paymentStreamCount)
			paymentStream = varSeed.PaymentStreamNRT[nPaymentStream]
		*/

		// select a paymentstream based on config file value
		paymentStream = vGeneral.Sourcesystem

		if paymentStream == "EFT" {
			TransactionTypesCount := len(varSeed.TransactionTypeNRT.EFT) - 1
			nTransactionTypesCount := gofakeit.Number(0, TransactionTypesCount)
			TransactionTypeNRT = varSeed.TransactionTypeNRT.EFT[nTransactionTypesCount].Name
			msgType = "EFT"

		} else if paymentStream == "ACD" {
			TransactionTypesCount := len(varSeed.TransactionTypeNRT.ACD) - 1
			nTransactionTypesCount := gofakeit.Number(0, TransactionTypesCount)
			TransactionTypeNRT = varSeed.TransactionTypeNRT.ACD[nTransactionTypesCount].Name
			msgType = "900000"

		} else if paymentStream == "RTC" {
			TransactionTypesCount := len(varSeed.TransactionTypeNRT.RTC) - 1
			nTransactionTypesCount := gofakeit.Number(0, TransactionTypesCount)
			TransactionTypeNRT = varSeed.TransactionTypeNRT.RTC[nTransactionTypesCount].Name
			msgType = "RTCCT"

		}
		jDebtorBank, _ = findTenant(varSeed.Tenants.Nrt, jDebtorAccount.TenantId)
		jCreditorBank, _ = findTenant(varSeed.Tenants.Nrt, jCreditorAccount.TenantId)

		localInstrumentCount := len(varSeed.LocalInstrument.HIST) - 1
		nlocalInstrumentCount := gofakeit.Number(0, localInstrumentCount)
		localInstrument = varSeed.LocalInstrument.HIST[nlocalInstrumentCount].Name

	} else { // RPP
		jDebtorBank, _ = findTenant(varSeed.Tenants.Rt, jDebtorAccount.TenantId)
		jCreditorBank, _ = findTenant(varSeed.Tenants.Rt, jCreditorAccount.TenantId)

		localInstrumentCount := len(varSeed.LocalInstrument.RPP) - 1
		nlocalInstrumentCount := gofakeit.Number(0, localInstrumentCount)
		localInstrument = varSeed.LocalInstrument.RPP[nlocalInstrumentCount].Value

	}

	// find FIB Id for the debtor and creditor bank
	DebtorFIBranchId = strconv.Itoa(gofakeit.Number(jDebtorBank.BranchRangeStart, jDebtorBank.BranchRangeEnd))
	CreditorFIBranchId = strconv.Itoa(gofakeit.Number(jCreditorBank.BranchRangeStart, jCreditorBank.BranchRangeEnd))

	txnId = uuid.New().String()
	eventTime = time.Now().Format("2006-01-02T15:04:05")

	requestExecutionDate = time.Now().Format("2006-01-02")
	settlementDate = time.Now().Format("2006-01-02")
	paymentClearingSystemReference = uuid.New().String()
	paymentRef = paymentClearingSystemReference
	remittanceId = paymentClearingSystemReference

	if vGeneral.Datamode == "hist" {

		// 2 x NRT records/events

		t_OutboundPayment = map[string]interface{}{
			"accountAgentId":                 jDebtorBank.TenantId,
			"accountId":                      jDebtorAccount.AccountNumber,
			"accountIdCode":                  jDebtorAccount.AccountIDCode, // Type of Account
			"accountNumber":                  jDebtorAccount.AccountNumber,
			"amount":                         t_amount,
			"chargeBearer":                   "SLEV",
			"counterpartyAgentId":            jCreditorBank.TenantId,
			"counterpartyId":                 jCreditorAccount.AccountNumber,
			"counterpartyIdCode":             jCreditorAccount.AccountIDCode, // Type of Account
			"counterpartyNumber":             jCreditorAccount.AccountNumber,
			"creationDate":                   time.Now().Format("2006-01-02T15:04:05"),
			"destinationCountry":             "ZAF",
			"direction":                      "outbound",
			"eventId":                        uuid.New().String(),
			"eventTime":                      eventTime,
			"eventType":                      "paymentNRT",
			"fromFIBranchId":                 DebtorFIBranchId,
			"fromId":                         jDebtorBank.TenantId,
			"localInstrument":                localInstrument, // aka Record ID's
			"msgStatus":                      "Settlement",
			"msgType":                        msgType,
			"msgStatusReason":                "JNL_ACQ.responseCode",
			"numberOfTransactions":           1,
			"paymentClearingSystemReference": paymentClearingSystemReference,
			"paymentMethod":                  "TRF",
			"paymentReference":               paymentRef,
			"remittanceId":                   remittanceId,
			"requestExecutionDate":           requestExecutionDate,
			"schemaVersion":                  1,
			"settlementClearingSystemCode":   paymentStream,
			"settlementDate":                 settlementDate,
			"settlementMethod":               "CLRG",
			"tenantId":                       jDebtorAccount.TenantId,
			"toFIBranchId":                   CreditorFIBranchId,
			"toId":                           jCreditorBank.TenantId,
			"totalAmount":                    t_amount,
			"transactionId":                  txnId,
			"transactionType":                TransactionTypeNRT,
			"verificationResult":             "SUCC",
			"usercode":                       "0000",
		}

		t_InboundPayment = map[string]interface{}{
			"accountAgentId":                 jCreditorBank.TenantId,
			"accountId":                      jCreditorAccount.AccountNumber,
			"accountIdCode":                  jCreditorAccount.AccountIDCode, // Type of Account
			"accountNumber":                  jCreditorAccount.AccountNumber,
			"amount":                         t_amount,
			"chargeBearer":                   "SLEV",
			"counterpartyAgentId":            jDebtorBank.TenantId,
			"counterpartyId":                 jDebtorAccount.AccountNumber,
			"counterpartyIdCode":             jDebtorAccount.AccountIDCode, // Type of Account
			"counterpartyNumber":             jDebtorAccount.AccountNumber,
			"creationDate":                   time.Now().Format("2006-01-02T15:04:05"),
			"destinationCountry":             "ZAF",
			"direction":                      "inbound",
			"eventId":                        uuid.New().String(),
			"eventTime":                      eventTime,
			"eventType":                      "paymentNRT",
			"fromFIBranchId":                 DebtorFIBranchId,
			"fromId":                         jDebtorBank.TenantId,
			"localInstrument":                localInstrument, // aka Record ID's
			"msgStatus":                      "Settlement",
			"msgType":                        msgType,
			"msgStatusReason":                "JNL_ACQ.responseCode",
			"numberOfTransactions":           1,
			"paymentClearingSystemReference": paymentClearingSystemReference,
			"paymentMethod":                  "TRF",
			"paymentReference":               paymentRef,
			"remittanceId":                   remittanceId,
			"requestExecutionDate":           requestExecutionDate,
			"schemaVersion":                  1,
			"settlementClearingSystemCode":   paymentStream,
			"settlementDate":                 settlementDate,
			"settlementMethod":               "CLRG",
			"tenantId":                       jCreditorBank.TenantId,
			"toFIBranchId":                   CreditorFIBranchId,
			"toId":                           jCreditorBank.TenantId,
			"totalAmount":                    t_amount,
			"transactionId":                  txnId,
			"transactionType":                TransactionTypeNRT,
			"verificationResult":             "SUCC",
			"usercode":                       "0000",
		}

	} else { // RPP
		// 1 NRT and 1 RT record/event

		// Outbound => paymentNRT
		// Inbound => paymentRT

		// Account => Debtor
		// CounterParty => Creditor

		t_OutboundPayment = map[string]interface{}{
			"accountAgentId":                    jDebtorBank.TenantId,
			"accountId":                         jDebtorAccount.AccountNumber,
			"accountIdCode":                     jDebtorAccount.AccountIDCode, // Type of Account
			"accountNumber":                     jDebtorAccount.AccountNumber,
			"amount":                            t_amount,
			"chargeBearer":                      "SLEV",
			"counterpartyAgentId":               jCreditorBank.TenantId,
			"counterpartyId":                    jCreditorAccount.AccountNumber,
			"counterpartyIdCode":                jCreditorAccount.AccountIDCode, // Type of Account
			"counterpartyNumber":                jCreditorAccount.AccountNumber,
			"creationDate":                      time.Now().Format("2006-01-02T15:04:05"),
			"destinationCountry":                "ZAF",
			"direction":                         "outbound",
			"eventId":                           uuid.New().String(),
			"eventTime":                         eventTime,
			"eventType":                         "paymentNRT",
			"fromFIBranchId":                    DebtorFIBranchId,
			"fromId":                            jDebtorBank.TenantId,
			"localInstrument":                   localInstrument, // Record ID's
			"msgStatus":                         "New",
			"msgType":                           "CRTRF",
			"msgStatusReason":                   "JNL_ACQ.responseCode",
			"numberOfTransactions":              1,
			"paymentClearingSystemReference":    paymentClearingSystemReference,
			"paymentMethod":                     "TRF",
			"paymentReference":                  paymentRef,
			"remittanceId":                      remittanceId,
			"requestExecutionDate":              requestExecutionDate,
			"schemaVersion":                     1,
			"settlementClearingSystemCode":      "RPP",
			"settlementDate":                    settlementDate,
			"settlementMethod":                  "CLRG",
			"tenantId":                          jDebtorAccount.TenantId,
			"toFIBranchId":                      CreditorFIBranchId,
			"toId":                              jCreditorBank.TenantId,
			"totalAmount":                       t_amount,
			"transactionId":                     txnId,
			"transactionType":                   "MTUP",
			"verificationResult":                "SUCC",
			"instructedAgentId":                 jDebtorBank.Bicfi,
			"instructingAgentId":                jCreditorBank.Bicfi,
			"intermediaryAgent1Id":              jDebtorBank.Bicfi,
			"intermediaryAgent2Id":              jCreditorBank.Bicfi,
			"ultimateAccountName":               jCreditorAccount.Name,
			"ultimateCounterpartyName":          jDebtorAccount.Name,
			"unstructuredRemittanceInformation": paymentClearingSystemReference,
		}

		// Account => Creditor
		// CounterParty => Debtor

		chargeBearersCount := len(varSeed.ChargeBearers) - 1
		nChargeBearers := gofakeit.Number(0, chargeBearersCount)
		chargeBearers := varSeed.ChargeBearers[nChargeBearers]

		//settlementMethodCount := len(varSeed.SettlementMethod) - 1
		//nSettlementMethod := gofakeit.Number(0, settlementMethodCount)

		t_InboundPayment = map[string]interface{}{
			"accountAgentId":    jCreditorBank.Bicfi,            // Bank Bicfi
			"accountId":         jCreditorAccount.AccountNumber, // Bank Acc Number
			"accountIdCode":     jCreditorAccount.AccountIDCode,
			"accountNumber":     jCreditorAccount.AccountNumber,
			"accountBICFI":      jCreditorBank.Bicfi,
			"accountProxyId":    jCreditorAccount.ProxyId,
			"accountProxyType":  jCreditorAccount.ProxyType,
			"accountDomain":     jCreditorAccount.ProxyDomain,
			"accountCustomerId": jCreditorAccount.AccountNumber,
			"accountAddress": types.TAddress{ // Payer - Payee Account
				AddressLine1:       jCreditorAccount.Address.AddressLine1, // CTT.creditor.streetName
				AddressLine2:       jCreditorAccount.Address.AddressLine2, // CTT.creditor.buildingNumber and buildingName
				TownName:           jCreditorAccount.Address.TownName,
				CountrySubDivision: jCreditorAccount.Address.CountrySubDivision,
				Country:            jCreditorAccount.Address.Country,
				PostalCode:         jCreditorAccount.Address.PostalCode,
				FullAddress:        jCreditorAccount.Address.FullAddress,
			},
			"accountName": types.TName{
				FullName:   jCreditorAccount.Name.FullName, // CTT.Creditor or CTT.Debtor
				NamePrefix: jCreditorAccount.Name.NamePrefix,
				Surname:    jCreditorAccount.Name.Surname,
			},
			"amount":                 t_amount,
			"chargeBearer":           chargeBearers,
			"counterpartyAgentId":    jDebtorBank.Bicfi, // Counterparty Bank
			"counterpartyId":         jDebtorAccount.AccountNumber,
			"counterpartyIdCode":     jDebtorAccount.AccountIDCode,
			"counterpartyNumber":     jDebtorAccount.AccountNumber,
			"counterpartyBICFI":      jDebtorBank.Bicfi,
			"counterpartyProxyId":    jDebtorAccount.ProxyId,
			"counterpartyProxyType":  jDebtorAccount.ProxyType,
			"counterpartyDomain":     jDebtorAccount.ProxyDomain,
			"counterpartyCustomerId": jDebtorAccount.AccountNumber,
			"counterpartyAddress": types.TAddress{ // Payer - Payee
				AddressLine1:       jDebtorAccount.Address.AddressLine1, // CTT.creditor.streetName
				AddressLine2:       jDebtorAccount.Address.AddressLine2, // CTT.creditor.buildingNumber and buildingName
				TownName:           jDebtorAccount.Address.TownName,
				CountrySubDivision: jDebtorAccount.Address.CountrySubDivision,
				Country:            jDebtorAccount.Address.Country,
				PostalCode:         jDebtorAccount.Address.PostalCode,
				FullAddress:        jDebtorAccount.Address.FullAddress,
			},
			"counterpartyName": types.TName{
				FullName:   jDebtorAccount.Name.FullName, // CTT.Creditor or CTT.Debtor
				NamePrefix: jDebtorAccount.Name.NamePrefix,
				Surname:    jDebtorAccount.Name.Surname,
			},
			"creationDate": time.Now().Format("2006-01-02T15:04:05"),
			//"customerId":                        jCreditorAccount.AccountNumber,
			"destinationCountry":                "ZAF",
			"direction":                         "inbound",
			"eventId":                           uuid.New().String(),
			"eventTime":                         eventTime,
			"eventType":                         "paymentRT",
			"fromId":                            jDebtorBank.TenantId,
			"localInstrument":                   localInstrument, // pick by LocalInstrumentRt, if pay buy proxy then no creditor account number
			"msgStatus":                         "New",
			"msgType":                           "CRTRF",
			"numberOfTransactions":              1,
			"paymentClearingSystemReference":    paymentClearingSystemReference,
			"paymentMethod":                     "TRF", // Hard coded CHK Cheque / TRF Transfer
			"paymentReference":                  paymentRef,
			"requestExecutionDate":              requestExecutionDate,
			"schemaVersion":                     1,
			"settlementClearingSystemCode":      "RPP",
			"settlementDate":                    settlementDate,
			"settlementMethod":                  "CLRG", // hard coded for now... -> varSeed.SettlementMethod[nSettlementMethod],
			"tenantId":                          jCreditorBank.TenantId,
			"toId":                              jCreditorBank.TenantId,
			"transactionId":                     txnId,
			"transactionType":                   "MTUP",
			"verificationResult":                "SUCC",
			"instructedAgentId":                 jDebtorBank.Bicfi,
			"instructingAgentId":                jCreditorBank.Bicfi,
			"intermediaryAgent1Id":              jDebtorBank.Bicfi,
			"intermediaryAgent2Id":              jCreditorBank.Bicfi,
			"ultimateAccountName":               jCreditorAccount.Name,
			"ultimateCounterpartyName":          jDebtorAccount.Name,
			"unstructuredRemittanceInformation": paymentClearingSystemReference,
		}
	}

	return t_OutboundPayment, t_InboundPayment, nil
}

func contructFinTransactionFromFile(varRec string) (t_OutboundPayment map[string]interface{}, t_InboundPayment map[string]interface{}, err error) {

	var objs interface{}

	// read our opened jsonFile as a byte array.
	byteValue, err := os.ReadFile(varRec)
	if err != nil {
		x := fmt.Sprintf("ReadFile error %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, nil, err

	}

	// we unmarshal our byteArray which contains our
	// jsonFile's content into 'users' which we defined above
	err = json.Unmarshal([]byte(byteValue), &objs)
	if err != nil {
		x := fmt.Sprintf("Unmarshall error %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, nil, err

	}

	// Ensure that it is an array of objects.
	objArr, ok := objs.([]interface{})
	if !ok {
		err = errors.New("expected an array of objects")
		grpcLog.Errorln(err)
		return nil, nil, err
	}

	// Handle each object as a map[string]interface{}.
	for i, obj := range objArr {
		obj, ok := obj.(map[string]interface{})
		if !ok {
			x := fmt.Sprintf("expected type map[string]interface{}, got %s", reflect.TypeOf(objArr[i]))
			err = errors.New(x)
			grpcLog.Errorln(err)
			return nil, nil, err
		}
		if i == 0 {
			t_InboundPayment = obj

		}
		if i == 1 {
			t_OutboundPayment = obj

		}
	}

	txnId := uuid.New().String()
	eventTime := time.Now().Format("2006-01-02T15:04:05")
	creationDate := time.Now().Format("2006-01-02T15:04:05")
	requestExecutionDate := time.Now().Format("2006-01-02")
	settlementDate := time.Now().Format("2006-01-02")

	// we update/refresh the eventID & eventTime, to ensure we don't get duplicate (and make it a payment in) id's at POST time
	t_OutboundPayment["eventId"] = uuid.New().String()
	t_InboundPayment["eventId"] = uuid.New().String()

	t_OutboundPayment["transactionId"] = txnId
	t_InboundPayment["transactionId"] = txnId

	t_OutboundPayment["eventTime"] = eventTime
	t_InboundPayment["eventTime"] = eventTime

	t_InboundPayment["creationDate"] = creationDate
	t_OutboundPayment["creationDate"] = creationDate

	if t_InboundPayment["eventType"] == "paymentRT" || t_InboundPayment["eventType"] == "paymentNRT" || t_OutboundPayment["eventType"] == "paymentRT" || t_OutboundPayment["eventType"] == "paymentNRT" {

		t_InboundPayment["requestExecutionDate"] = requestExecutionDate
		t_InboundPayment["settlementDate"] = settlementDate

		t_OutboundPayment["requestExecutionDate"] = requestExecutionDate
		t_OutboundPayment["settlementDate"] = settlementDate
	}

	return t_OutboundPayment, t_InboundPayment, nil
}

// used together with isJSON to check the file contents to ensure it's a valid JSON structure.
func ReadJSONFile(varRec string) ([]byte, error) {

	// Let's first read the `config.json` file
	content, err := os.ReadFile(varRec)
	if err != nil {
		x := fmt.Sprintf("Error when opening file: %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, err
	}
	return content, nil
}

func isJSON(content []byte) (isJson bool) {

	var t_Payment interface{}

	err := json.Unmarshal(content, &t_Payment)
	if err != nil {
		isJson = false

	} else {
		isJson = true

	}

	return isJson
}

// Return list of files located in input_path to be repackaged as JSON payloads and posted onto the API endpoint
func fetchJSONRecords(input_path string) (records map[int]string, count int, err error) {

	var m = make(map[int]string)

	count = 0

	// https://yourbasic.org/golang/list-files-in-directory/
	// Use the os.ReadDir function in package os. It returns a sorted slice containing elements of type os.FileInfo.
	files, err := os.ReadDir(input_path)
	if err != nil {
		x := fmt.Sprintf("Problem retrieving list of input files: %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, 0, err
	}

	for _, file := range files {
		if vGeneral.Debuglevel > 2 {
			grpcLog.Info("File found    ", file.Name())

		}
		if !file.IsDir() {
			// Lets just append .json files to the array of files to be processed
			if strings.Split(file.Name(), ".")[1] == "json" {

				if vGeneral.Debuglevel > 2 {
					grpcLog.Info("File appended ", file.Name())
				}

				m[count] = file.Name()
				count++
			}

		}
	}

	records = m

	return records, count, err
}

func ConstructHTTPClient() (*http.Client, error) {

	var client *http.Client

	// Create client with Cert once
	// https://stackoverflow.com/questions/38822764/how-to-send-a-https-request-with-a-certificate-golang

	caCert, err := os.ReadFile(vGeneral.Cert_file)
	if err != nil {

		x := fmt.Sprintf("Problem reading: %s Error: %s", vGeneral.Cert_file, err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, err

	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair(vGeneral.Cert_file, vGeneral.Cert_key)
	if err != nil {
		x := fmt.Sprintf("Problem with LoadX509KeyPair: %s Error: %s", vGeneral.Cert_key, err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, err

	}

	if vGeneral.ProxyURL_enabled == 1 {
		grpcLog.Info("* HTTP Client With Proxy Server defined")

		proxyURL, err := url.Parse(vGeneral.ProxyURL)
		if err != nil {
			x := fmt.Sprintf("Failed to configure HTTP Proxy server, Error: %s", err)
			err = errors.New(x)
			grpcLog.Errorln(err)
			return nil, err

		}

		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:            caCertPool,
					InsecureSkipVerify: true, // Self Signed cert
					Certificates:       []tls.Certificate{cert},
				},
				Proxy: http.ProxyURL(proxyURL),
			},
		}

	} else {
		grpcLog.Info("* HTTP Client Without Proxy Server defined")

		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:            caCertPool,
					InsecureSkipVerify: true, // Self Signed cert
					Certificates:       []tls.Certificate{cert},
				},
			},
		}
	}

	return client, nil

}

func httpCALL(Bytes []byte, url string, client *http.Client) (Response *http.Response, err error) {

	// https://golangtutorial.dev/tips/http-post-json-go/
	Request, err := http.NewRequest("POST", url, bytes.NewBuffer(Bytes))
	if err != nil {
		x := fmt.Sprintf("http.NewRequest error: %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, err

	}
	Request.Header.Set("Content-Type", "application/json; charset=UTF-8")

	httpResponse, err := client.Do(Request)
	if err != nil {
		x := fmt.Sprintf("client.Do error: %s", err)
		err = errors.New(x)
		grpcLog.Errorln(err)
		return nil, err

	}

	return httpResponse, nil
}

func RiskScoreExtract(t_Response map[string]interface{}) (riskScore float64, err error) {

	// Score Extraction
	var vScore float64
	vScore = 0.0

	// Access the nested array
	entities, ok := t_Response["entities"].([]interface{})
	if !ok {
		err = errors.New("error accessing entities")
		grpcLog.Errorln(err)
		return 0, err

	}

	// entities are an array of json objects
	for _, entity := range entities {
		entityMap, ok := entity.(map[string]interface{})
		if !ok {
			err = errors.New("error accessing entity details")
			grpcLog.Errorln(err)
			return 0, err
		}

		// Access the details map for each language
		overallScores, ok := entityMap["overallScore"].(map[string]interface{})
		if !ok {
			err = errors.New("error accessing overall score map details")
			grpcLog.Errorln(err)
		}

		for _, value := range overallScores {
			if value != nil {
				f, ok := value.(float64) // this won't panic because there's an ok
				if ok {
					if f > float64(vScore) {
						vScore = f
					}
				}
			}
		}
	}

	return vScore, nil

}

// Big worker... This si where everything happens.
func runLoader(arg string) {

	// Initialize the vGeneral struct variable - This holds our configuration settings.
	vGeneral = loadConfig(arg)

	// Lets get Seed Data from the specified seed file
	varSeed = loadSeed(vGeneral.SeedFile)

	if vGeneral.Prometheus_enabled == 1 {
		pusher = push.New(vGeneral.Prometheus_push_gateway, "pushgateway").Gatherer(reg)
	}

	// Build our HTTP Client to use for the API calls.
	client, err := ConstructHTTPClient()
	if err != nil {
		os.Exit(1)

	}

	if vGeneral.Debuglevel > 0 {
		grpcLog.Info("**** LETS GO Processing ****")
		grpcLog.Infoln("")

	}

	////////////////////////////////////////////////////////////////////////
	// Lets fecth the records that need to be pushed to the fs api end point
	var todo_count = 0
	var returnedRecs map[int]string
	if vGeneral.Json_from_file == 0 { // Build Fake Record - atm we're generating the data, eventually we might fetch via SQL

		// As we're faking it:
		todo_count = vGeneral.Testsize // this will be recplaced by the value of todo_count from above.

	} else { // Build Record set from data fetched from JSON files in input_path

		// this will return an map of files names, each being a JSON document
		returnedRecs, todo_count, err = fetchJSONRecords(vGeneral.Input_path)
		if err != nil {
			os.Exit(1)

		}

		if vGeneral.Debuglevel > 1 {
			grpcLog.Infoln("Checking input event files (Making sure it's valid JSON)...")
			grpcLog.Infoln("")

		}

		var weFailed bool = false
		for count := 0; count < todo_count; count++ {

			// Build the entire JSON Payload document, either a fake record or from a input/scenario JSON file

			filename := fmt.Sprintf("%s%s%s", vGeneral.Input_path, pathSep, returnedRecs[count])

			contents, err := ReadJSONFile(filename)
			if err != nil {
				os.Exit(1)

			}

			// We internall to isJSON uses the existence of a error to determine if the file is valid JSON.
			if !isJSON(contents) {
				weFailed = true
				grpcLog.Infoln(filename, "=> FAIL")

			} else {
				grpcLog.Infoln(filename, "=> Pass")

			}

		}
		// We're saying we want to use records from input files but not all the files are valid JSON, die die die... ;)
		if weFailed {
			os.Exit(1)

		}
		grpcLog.Infoln("")

	}

	var vService string
	if vGeneral.Datamode == "rpp" {
		vService = "rpp"

	} else {
		vService = vGeneral.Sourcesystem
	}

	if vGeneral.Prometheus_enabled == 1 {
		m.api_pmnt_info.With(prometheus.Labels{"hostname": vGeneral.Hostname, "service": vService}).Set(float64(todo_count))

		// Add is used here rather than Push to not delete a previously pushed
		// success timestamp in case of a failure of this backup.
		if err := pusher.Add(); err != nil {
			grpcLog.Errorln("Could not push to Pushgateway:", err)

		}
	}

	if vGeneral.Debuglevel > 1 {
		grpcLog.Infoln("Number of records to Process", todo_count) // just doing this to prefer a unused error

	}

	// now we loop through the results, building a json document based on FS requirements and then post it, for this code I'm posting to
	// Confluent Kafka topic, but it's easy to change to have it post to a API endpoint.

	// this is to keep record of the total batch run time
	vStart := time.Now()

	for count := 0; count < todo_count; count++ {

		reccount := fmt.Sprintf("%v", count+1)

		if vGeneral.Debuglevel > 1 {
			grpcLog.Infoln("")
			grpcLog.Infoln("Record                        :", reccount)

		}

		// We're going to time every record and push that to prometheus
		txnStart := time.Now()

		// We creating fake data so we will have 2 events to deal with
		var t_InboundPayload map[string]interface{}
		var t_OutboundPayload map[string]interface{}
		var InboundBytes []byte
		var OutboundBytes []byte
		var jsonDataInboundResponsebody []byte
		var jsonDataOutboundResponsebody []byte
		var tInboundBody map[string]interface{}
		var tOutboundBody map[string]interface{}

		// Build the entire JSON Payload document, either a fake record or from a input/scenario JSON file
		if vGeneral.Json_from_file == 0 { // Build Fake Record

			// They are just to different to have kept in one function, so split them into 2 seperate specific use case functions.
			// we use the same return structure as contructTransactionFromJSONFile()
			t_OutboundPayload, t_InboundPayload, err = constructFakeFinTransaction()
			if err != nil {
				os.Exit(1)

			}
		} else {
			// We're reading data from files, so simply post data per file/payload

			// returnedRecs is a map of file names, each filename is 2 JSON documents, each of which is a FS Payment (or addProxy) event,
			// At this point we simply post the events onto the FS end point, and record the response.

			filename := fmt.Sprintf("%s%s%s", vGeneral.Input_path, pathSep, returnedRecs[count])

			if vGeneral.Debuglevel > 2 {
				grpcLog.Infoln("Source Event                  :", filename)

			}
			t_OutboundPayload, t_InboundPayload, err = contructFinTransactionFromFile(filename)
			if err != nil {
				os.Exit(1)

			}
		}

		if vGeneral.Debuglevel > 1 {

			// We can display the t_InboundPayload values here as we assigned the same values, inbound payment to be before outbound
			// to both the inbound and outbound Payloads
			grpcLog.Infoln("transactionId assigned        :", t_InboundPayload["transactionId"])
			grpcLog.Infoln("eventTime assigned            :", t_InboundPayload["eventTime"])
			grpcLog.Infoln("creationDate assigned         :", t_InboundPayload["creationDate"])

			if t_InboundPayload["eventType"] == "paymentNRT" || t_InboundPayload["eventType"] == "paymentRT" {
				// payment*
				grpcLog.Infoln("requestExecutionDate assigned :", t_InboundPayload["requestExecutionDate"])
				grpcLog.Infoln("settlementDate assigned       :", t_InboundPayload["settlementDate"])

				grpcLog.Infoln("")
				grpcLog.Infoln("Inbound eventId assigned      :", t_InboundPayload["eventId"])
				grpcLog.Infoln("Outbound eventId assigned     :", t_OutboundPayload["eventId"])

			} else {
				// addPayee*
				grpcLog.Infoln("")
				grpcLog.Infoln("Outbound                      :")
				grpcLog.Infoln("eventId assigned              :", t_OutboundPayload["eventId"])

				grpcLog.Infoln("")
				grpcLog.Infoln("Inbound                       :")
				grpcLog.Infoln("eventId assigned              :", t_InboundPayload["eventId"])

			}

		}

		InboundBytes, err = json.Marshal(t_InboundPayload)
		if err != nil {
			grpcLog.Errorln("Marchalling error: ", err)

		}

		OutboundBytes, err = json.Marshal(t_OutboundPayload)
		if err != nil {
			grpcLog.Errorln("Marchalling error: ", err)

		}

		if vGeneral.Debuglevel > 1 && vGeneral.Echojson == 1 {

			if t_InboundPayload["eventType"] == "paymentNRT" || t_InboundPayload["eventType"] == "paymentRT" {

				grpcLog.Infoln("Inbound Payload   	:")
				prettyJSON(string(InboundBytes))

				grpcLog.Infoln("")

				grpcLog.Infoln("Outbound Payload   	:")
				prettyJSON(string(OutboundBytes))

			} else {
				grpcLog.Infoln("Outbound Payload   	:")
				prettyJSON(string(OutboundBytes))

				grpcLog.Infoln("")

				grpcLog.Infoln("Inbound Payload   	:")
				prettyJSON(string(InboundBytes))
			}

		}

		// At this point we have 2 Payloads, either fake or from source files.
		// Now lets http post them
		if vGeneral.Call_fs_api == 1 { // POST to API endpoint

			grpcLog.Info("")
			grpcLog.Info("Call API Flow")
			grpcLog.Info("")

			var vParticipant string
			var vLocalInstrument string
			var apiInboundStart time.Time
			var apiOutboundStart time.Time
			var apiInboundEnd float64
			var apiOutboundEnd float64
			var InboundResponse *http.Response
			var OutboundResponse *http.Response

			if t_InboundPayload["eventType"] == "paymentNRT" || t_InboundPayload["eventType"] == "paymentRT" {

				if vGeneral.Debuglevel > 0 {
					grpcLog.Infoln("payment* Flow")
				}

				// We're doing payments, so:

				// Inbound Call Section
				// 	paymentRT before paymentNRT
				//
				// 	paymentRT will have a 200 if successful & paymentNRT will have a 204 if successful.

				apiInboundStart = time.Now()
				InboundResponse, err = httpCALL(InboundBytes, vGeneral.Httpposturl, client)
				if err != nil {
					os.Exit(1)

				}
				apiInboundEnd = time.Since(apiInboundStart).Seconds()
				defer InboundResponse.Body.Close()

				// We need to do 2 api calls, 1 each for outbound and inbound event.

				// Outbound Call Section
				// 	paymentNRT
				//
				// 	paymentNRT will have a 204 if successful

				apiOutboundStart = time.Now()
				OutboundResponse, err = httpCALL(OutboundBytes, vGeneral.Httpposturl, client)
				if err != nil {
					os.Exit(1)

				}
				apiOutboundEnd = time.Since(apiOutboundStart).Seconds()
				defer OutboundResponse.Body.Close()

			} else {

				if vGeneral.Debuglevel > 0 {
					grpcLog.Infoln("")
					grpcLog.Infoln("AddPayee* Flow")
				}
				// We're doing addPayee*, so:

				// Outbound Call Section
				// 	addPayeeRT followed by addPayeeNRT
				//
				// 	outbound addPayeeRT will have a 200 if successful, inbound will have a 204

				apiOutboundStart = time.Now()
				OutboundResponse, err = httpCALL(OutboundBytes, vGeneral.Httpposturl, client)
				if err != nil {
					os.Exit(1)

				}
				apiOutboundEnd = time.Since(apiOutboundStart).Seconds()
				defer OutboundResponse.Body.Close()

				// Inbound Call Section
				apiInboundStart = time.Now()
				InboundResponse, err = httpCALL(InboundBytes, vGeneral.Httpposturl, client)
				if err != nil {
					os.Exit(1)

				}
				apiInboundEnd = time.Since(apiInboundStart).Seconds()
				defer InboundResponse.Body.Close()

			}

			// http calls done, in required order.
			// Response Extract section,

			// Do something with all the output/response
			jsonDataInboundResponsebody, err = io.ReadAll(InboundResponse.Body)
			if err != nil {
				grpcLog.Errorln("Inbound Body -> io.ReadAll(InboundResponse.Body) error: ", err)

			}

			jsonDataOutboundResponsebody, err = io.ReadAll(OutboundResponse.Body)
			if err != nil {
				grpcLog.Errorln("Outbound Body -> io.ReadAll(OutboundResponse.Body) error: ", err)

			}

			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("")
				grpcLog.Infoln("Inbound API Call Time         :", apiInboundEnd, "Sec")
				grpcLog.Infoln("Outbound API Call Time        :", apiOutboundEnd, "Sec")

				if vGeneral.Debuglevel > 2 {
					grpcLog.Infoln("")
					grpcLog.Infoln("Inbound response Headers      :", InboundResponse.Header)
					grpcLog.Infoln("")
					grpcLog.Infoln("Outbound response Headers     :", OutboundResponse.Header)

				}
			}

			grpcLog.Infoln("")
			grpcLog.Infoln("Inbound response Status       :", InboundResponse.Status)
			grpcLog.Infoln("Outbound response Status      :", OutboundResponse.Status)
			grpcLog.Infoln("")

			// Define a map to hold the JSON data
			var inboundResponsebodyMap map[string]interface{}
			_ = json.Unmarshal(jsonDataInboundResponsebody, &inboundResponsebodyMap)

			var outboundResponsebodyMap map[string]interface{}
			_ = json.Unmarshal(jsonDataOutboundResponsebody, &outboundResponsebodyMap)

			if InboundResponse.Status == "200 OK" { // paymentRT

				// it's a paymentRT (only Inbound event that response with a 200 is paymentRT event)
				if vGeneral.Prometheus_enabled == 1 && t_InboundPayload["eventType"].(string) == "paymentRT" {

					// Modularized
					vScore, err := RiskScoreExtract(inboundResponsebodyMap)
					if err != nil {
						grpcLog.Errorln(err)

					}

					// Remove shortly.
					/*
						// Score Extraction
						var vScore float64
						vScore = 0.0

						// Access the nested array
						entities, ok := inboundResponsebodyMap["entities"].([]interface{})
						if !ok {
							err = errors.New("error accessing entities")
							grpcLog.Errorln(err)

						}

						// entities are an array of json objects
						for _, entity := range entities {
							entityMap, ok := entity.(map[string]interface{})
							if !ok {
								err = errors.New("error accessing entity details")
								grpcLog.Errorln(err)
							}

							// Access the details map for each language
							overallScores, ok := entityMap["overallScore"].(map[string]interface{})
							if !ok {
								err = errors.New("error accessing overall score map details")
								grpcLog.Errorln(err)
							}

							for _, value := range overallScores {
								if value != nil {
									f, ok := value.(float64) // this won't panic because there's an ok
									if ok {
										if f > float64(vScore) {
											vScore = f
										}
									}
								}
							}
						} */

					if vGeneral.Debuglevel > 2 {
						grpcLog.Infoln("overallScore for paymentRT    :", vScore)

					}

					vParticipant = t_InboundPayload["tenantId"].(string)
					vLocalInstrument = t_InboundPayload["localInstrument"].(string)
					xScore := fmt.Sprintf("%v", vScore)

					m.api_pmnt_duration.With(prometheus.Labels{
						"hostname":       vGeneral.Hostname,
						"msg_type":       t_InboundPayload["eventType"].(string),
						"service":        vService,
						"participant":    vParticipant,
						"direction":      "inbound",
						"payment_method": vLocalInstrument,
						"score":          xScore}).Observe(apiInboundEnd)

				}

				if vGeneral.Debuglevel > 2 {

					grpcLog.Infoln("Inbound response Body         : paymentRT")

				}

				// lets build a body of the header and some additional information
				tInboundBody = map[string]interface{}{
					"transactionId":   t_InboundPayload["transactionId"],
					"eventId":         t_InboundPayload["eventId"],
					"eventType":       t_InboundPayload["eventType"],
					"responseStatus":  InboundResponse.Status,
					"responseHeaders": InboundResponse.Header,
					"responseBody":    inboundResponsebodyMap,
					"processTime":     time.Now().UTC(),
				}

			} else if InboundResponse.Status == "204 No Content" {

				// it's either a paymentNRT or addPayeeNRT

				if vGeneral.Prometheus_enabled == 1 {

					vParticipant = t_InboundPayload["tenantId"].(string)
					vScore := fmt.Sprintf("%v", "0.0") // for NRT payloads we simply push a 0 score, to comply # variables for the prometheus object call

					if t_InboundPayload["eventType"].(string) == "paymentNRT" {

						vLocalInstrument = t_InboundPayload["localInstrument"].(string)

						m.api_pmnt_duration.With(prometheus.Labels{
							"hostname":       vGeneral.Hostname,
							"msg_type":       t_InboundPayload["eventType"].(string), // paymentNRT
							"service":        vService,
							"participant":    vParticipant,
							"direction":      "inbound",
							"payment_method": vLocalInstrument,
							"score":          vScore}).Observe(apiInboundEnd)

					} else {

						m.api_addpayee_duration.With(prometheus.Labels{
							"hostname":    vGeneral.Hostname,
							"msg_type":    t_InboundPayload["eventType"].(string), // AddPayeeNRT
							"service":     vService,
							"participant": vParticipant,
							"direction":   "inbound",
							"score":       vScore}).Observe(apiInboundEnd)
					}
				}

				if vGeneral.Debuglevel > 2 {
					if t_InboundPayload["eventType"].(string) == "paymentNRT" {

						grpcLog.Infoln("Inbound response Body         : paymentNRT")

					} else if t_InboundPayload["eventType"].(string) == "AddPayeeNRT" {

						grpcLog.Infoln("Inbound response Body         : AddPayeeNRT")

					}
				}

				tInboundBody = map[string]interface{}{
					"transactionId":   t_InboundPayload["transactionId"],
					"eventId":         t_InboundPayload["eventId"],
					"eventType":       t_InboundPayload["eventType"],
					"responseStatus":  InboundResponse.Status,
					"responseHeaders": InboundResponse.Header,
					"responseBody":    "paymentNRT",
					"processTime":     time.Now().UTC(),
				}

			} else {

				// oh sh$t, its not a success so now to try and build a body to fault fix later

				if vGeneral.Prometheus_enabled == 1 {

					vParticipant = t_InboundPayload["tenantId"].(string)

					if t_InboundPayload["eventType"].(string) == "paymentRT" || t_InboundPayload["eventType"].(string) == "paymentNRT" {

						m.err_pmnt_processed.With(prometheus.Labels{
							"hostname":       vGeneral.Hostname,
							"msg_type":       t_InboundPayload["eventType"].(string), // paymentRT or paymentNRT
							"service":        vService,
							"participant":    vParticipant,
							"direction":      "inbound",
							"payment_method": vLocalInstrument}).Inc()

					} else {

						m.err_addpayee_processed.With(prometheus.Labels{
							"hostname":    vGeneral.Hostname,
							"msg_type":    t_InboundPayload["eventType"].(string), // addPayeeNRT
							"service":     vService,
							"participant": vParticipant,
							"direction":   "inbound"}).Inc()
					}
				}

				if vGeneral.Debuglevel > 2 {

					grpcLog.Infoln("Inbound response Body         :", string(jsonDataInboundResponsebody))
					grpcLog.Infoln("Inbound response Result       : FAILED POST")

				}

				tInboundBody = map[string]interface{}{
					"transactionId":   t_InboundPayload["transactionId"],
					"eventId":         t_InboundPayload["eventId"],
					"eventType":       t_InboundPayload["eventType"],
					"responseResult":  "FAILED POST",
					"responseBody":    inboundResponsebodyMap,
					"responseStatus":  InboundResponse.Status,
					"responseHeaders": InboundResponse.Header,
					"processTime":     time.Now().UTC(),
				}
			}

			// Add is used here rather than Push to not delete a previously pushed
			// success timestamp in case of a failure of this backup.
			if vGeneral.Prometheus_enabled == 1 {
				if err := pusher.Add(); err != nil {
					grpcLog.Errorln("Could not push Inbound metrics to Pushgateway:", err)

				}
			}

			if OutboundResponse.Status == "200 OK" { // AddPayeeRT

				if vGeneral.Prometheus_enabled == 1 {

					// Confirm with FS if we need to extract the risk score for a addPayeeRT
					//

					vParticipant = t_OutboundPayload["tenantId"].(string)
					vScore := fmt.Sprintf("%v", "0.0")

					if t_OutboundPayload["eventType"].(string) == "addPayeeRT" {

						m.api_addpayee_duration.With(prometheus.Labels{
							"hostname":    vGeneral.Hostname,
							"msg_type":    t_OutboundPayload["eventType"].(string),
							"service":     vService,
							"participant": vParticipant,
							"direction":   "outbound",
							"score":       vScore}).Observe(apiOutboundEnd)

					}
				}

				// it's a addPayeeRT - SUCCESS
				// lets build a body of the header and some additional information
				if vGeneral.Debuglevel > 2 {

					grpcLog.Infoln("Outbound response Body        : addPayeeRT")

				}

				tOutboundBody = map[string]interface{}{
					"transactionId":   t_OutboundPayload["transactionId"],
					"eventId":         t_OutboundPayload["eventId"],
					"eventType":       t_OutboundPayload["eventType"],
					"responseStatus":  OutboundResponse.Status,
					"responseHeaders": OutboundResponse.Header,
					"responseBody":    outboundResponsebodyMap,
					"processTime":     time.Now().UTC(),
				}

			} else if OutboundResponse.Status == "204 No Content" { // paymentNRT

				if vGeneral.Prometheus_enabled == 1 {

					vParticipant = t_OutboundPayload["tenantId"].(string)

					if t_OutboundPayload["eventType"].(string) == "paymentNRT" {

						vLocalInstrument = t_OutboundPayload["localInstrument"].(string)
						vScore := fmt.Sprintf("%v", "0.0") // for NRT payloads we simply push a 0 score, to comply # variables for the prometheus object call

						m.api_pmnt_duration.With(prometheus.Labels{
							"hostname":       vGeneral.Hostname,
							"msg_type":       t_OutboundPayload["eventType"].(string),
							"service":        vService,
							"participant":    vParticipant,
							"direction":      "outbound",
							"payment_method": vLocalInstrument,
							"score":          vScore}).Observe(apiOutboundEnd)

					}
				}

				if vGeneral.Debuglevel > 2 {

					grpcLog.Infoln("Outbound response Body        : paymentNRT")
				}

				tOutboundBody = map[string]interface{}{
					"transactionId":   t_OutboundPayload["transactionId"],
					"eventId":         t_OutboundPayload["eventId"],
					"eventType":       t_OutboundPayload["eventType"],
					"responseStatus":  OutboundResponse.Status,
					"responseHeaders": OutboundResponse.Header,
					"responseBody":    "paymentNRT",
					"processTime":     time.Now().UTC(),
				}

			} else {

				// oh sh$t, its not a success so now to try and build a body to fault fix later

				if vGeneral.Prometheus_enabled == 1 {

					vParticipant = t_OutboundPayload["tenantId"].(string)

					if t_OutboundPayload["eventType"].(string) == "paymentNRT" {

						m.err_pmnt_processed.With(prometheus.Labels{
							"hostname":       vGeneral.Hostname,
							"msg_type":       t_OutboundPayload["eventType"].(string), // paymentNRT
							"service":        vService,
							"participant":    vParticipant,
							"direction":      "outbound",
							"payment_method": vLocalInstrument}).Inc()

					} else {

						m.err_addpayee_processed.With(prometheus.Labels{
							"hostname":    vGeneral.Hostname,
							"msg_type":    t_OutboundPayload["eventType"].(string), // addPayeeRT
							"service":     vService,
							"participant": vParticipant,
							"direction":   "outbound"}).Inc()
					}
				}

				if vGeneral.Debuglevel > 2 {

					grpcLog.Infoln("Outbound response Body        :", string(jsonDataOutboundResponsebody))
					grpcLog.Infoln("Outbound response Result      : FAILED POST")

				}

				tOutboundBody = map[string]interface{}{
					"transactionId":   t_OutboundPayload["transactionId"],
					"eventId":         t_OutboundPayload["eventId"],
					"eventType":       t_OutboundPayload["eventType"],
					"responseResult":  "FAILED POST",
					"responseBody":    string(jsonDataOutboundResponsebody),
					"responseStatus":  OutboundResponse.Status,
					"responseHeaders": OutboundResponse.Header,
					"processTime":     time.Now().UTC(),
				}
			}

			// Add is used here rather than Push to not delete a previously pushed
			// success timestamp in case of a failure of this backup.
			if vGeneral.Prometheus_enabled == 1 {
				if err := pusher.Add(); err != nil {
					grpcLog.Errorln("Could not push Outbound metrics to Pushgateway:", err)

				}
			}

		}
		// end of the Call_fs_api = 1 processing

		// Output Cycle
		//
		// We've posted the payloads and gotten the various forms of responses
		// and combined the FS response with a larger ..Payload to output to
		// screen and file.

		//
		// event if we post to FS API or not, we want isolated control if we output to the engineResponse json output file.

		// We have 2 steps here, first the original posted event, this is controlled by json_to_file,
		// the 2ne is a always print, which is the api post response
		TransactionId := t_InboundPayload["transactionId"]
		OutboundTagId := t_OutboundPayload["eventId"]
		InboundTagId := t_InboundPayload["eventId"]

		fileStart := time.Now()

		// I'm going to split this into 2 sections.
		// first is outputting the posted event data to a file <transaction id>-<event id>.json
		// the second is the http response received, which will go to <transaction id>-<event id>-out.json

		if vGeneral.Json_to_file == 1 {

			grpcLog.Info("")
			grpcLog.Info("JSON to File Flow")

			//...................................
			// Writing struct type to a JSON file
			//...................................
			// Writing
			// https://www.golangprograms.com/golang-writing-struct-to-json-file.html
			// https://www.developer.com/languages/json-files-golang/
			// Reading
			// https://medium.com/kanoteknologi/better-way-to-read-and-write-json-file-in-golang-9d575b7254f2

			// We need to do 2 api calls, 1 each for outbound and inbound event.

			// Inbound
			// The posted event
			loc_in := fmt.Sprintf("%s%s%s_%s-%s.json", vGeneral.Output_path, pathSep, reccount, TransactionId, InboundTagId)
			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("Inbound Output Event          :", loc_in)

			}

			fd_in, err := json.MarshalIndent(t_InboundPayload, "", " ")
			if err != nil {
				grpcLog.Errorln("MarshalIndent error", err)

			}

			err = os.WriteFile(loc_in, fd_in, 0644)
			if err != nil {
				grpcLog.Errorln("os.WriteFile error A", err)

			}

			// Outbound
			// The posted event
			loc_out := fmt.Sprintf("%s%s%s_%s-%s.json", vGeneral.Output_path, pathSep, reccount, TransactionId, OutboundTagId)
			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("Outbound Output Event         :", loc_out)

			}

			fd_out, err := json.MarshalIndent(t_OutboundPayload, "", " ")
			if err != nil {
				grpcLog.Errorln("MarshalIndent error", err)

			}

			err = os.WriteFile(loc_out, fd_out, 0644)
			if err != nil {
				grpcLog.Errorln("os.WriteFile error B", err)

			}

		}

		// Did we call the API endpoint above... if yes then do these steps
		// here we save the result to a file.
		if vGeneral.Call_fs_api == 1 {

			var loc_in string
			var loc_out string

			sourcefile := strings.Split(returnedRecs[count], ".")[0]

			// inbound engineResponse
			if vGeneral.Json_from_file == 1 {
				loc_in = fmt.Sprintf("%s%s%s-%s-%s-out.json", vGeneral.Output_path, pathSep, sourcefile, TransactionId, InboundTagId)

			} else {
				loc_in = fmt.Sprintf("%s%s%s_%s-%s-out.json", vGeneral.Output_path, pathSep, reccount, TransactionId, InboundTagId)

			}

			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("Inbound engineResponse file   :", loc_in)
				grpcLog.Infoln("")

			}

			fj_in, err := json.MarshalIndent(tInboundBody, "", " ")
			if err != nil {
				grpcLog.Errorln("MarshalIndent error", err)

			}

			err = os.WriteFile(loc_in, fj_in, 0644)
			if err != nil {
				grpcLog.Errorln("os.WriteFile error", err)

			}

			// outbound engineResponse
			if vGeneral.Json_from_file == 1 {
				loc_out = fmt.Sprintf("%s%s%s-%s-%s-out.json", vGeneral.Output_path, pathSep, sourcefile, TransactionId, OutboundTagId)

			} else {
				loc_out = fmt.Sprintf("%s%s%s_%s-%s-out.json", vGeneral.Output_path, pathSep, reccount, TransactionId, OutboundTagId)

			}
			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("Outbound engineResponse file  :", loc_out)

			}

			fj_out, err := json.MarshalIndent(tOutboundBody, "", " ")
			if err != nil {
				grpcLog.Errorln("MarshalIndent error", err)

			}

			err = os.WriteFile(loc_out, fj_out, 0644)
			if err != nil {
				grpcLog.Errorln("os.WriteFile error", err)

			}

			// lets report how long it took us to write data to output files
			if vGeneral.Debuglevel > 0 {
				grpcLog.Infoln("JSON to File                  :", time.Since(fileStart).Seconds(), "Sec")

			}
		}

		if vGeneral.Debuglevel > 0 {
			grpcLog.Infoln("Total Time                    :", time.Since(txnStart).Seconds(), "Sec")

		}

		//////////////////////////////////////////////////
		//
		// THIS IS SLEEP BETWEEN RECORD POSTS
		//
		// if 0 then sleep is disabled otherwise
		//
		// lets get a random value 0 -> vGeneral.sleep, then delay/sleep as up to that fraction of a second.
		// this mimics someone thinking, as if this is being done by a human at a keyboard, for batch file processing we don't have this.
		// ie if the user said 200 then it implies a randam value from 0 -> 200 milliseconds.
		//
		// USED TO SLOW THINGS DOWN
		//
		//////////////////////////////////////////////////

		if vGeneral.Sleep != 0 {
			n := rand.Intn(vGeneral.Sleep) // if vGeneral.sleep = 1000, then n will be random value of 0 -> 1000  aka 0 and 1 second
			if vGeneral.Debuglevel >= 2 {
				grpcLog.Infof("Going to sleep for            : %d Milliseconds\n", n)

			}
			time.Sleep(time.Duration(n) * time.Millisecond)
		}

	}

	if vGeneral.Debuglevel > 0 {
		grpcLog.Infoln("")
		grpcLog.Infoln("**** DONE Processing ****")
		grpcLog.Infoln("")

		if vGeneral.Debuglevel >= 1 {
			vEnd := time.Now()
			vElapse := vEnd.Sub(vStart)
			grpcLog.Infoln("Start                         : ", vStart)
			grpcLog.Infoln("End                           : ", vEnd)
			grpcLog.Infoln("Elapsed Time (Seconds)        : ", vElapse.Seconds())
			grpcLog.Infoln("Records Processed             : ", todo_count)
			grpcLog.Infoln(fmt.Sprintf("                              :  %.3f Txns/Second", float64(todo_count)/vElapse.Seconds()))
			grpcLog.Infoln(fmt.Sprintf("(x2 Txns)                     :  %.3f Events/Second", float64(todo_count)/vElapse.Seconds()*2))

			//		grpcLog.Infoln(fmt.Sprintf("Transactions # / second       :  %.3f Txns/Second", float64(todo_count)/vElapse.Seconds()))
			//		grpcLog.Infoln(fmt.Sprintf("Events # / second  (x2 Txns)  :  %.3f Events/Sec", float64(todo_count)/vElapse.Seconds()*2))

			grpcLog.Infoln("")
		}
	}

} // runLoader()

func main() {

	var arg string

	grpcLog.Info("****** Starting           *****")

	arg = os.Args[1]

	runLoader(arg)

	grpcLog.Info("****** Completed          *****")

}
