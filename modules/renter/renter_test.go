package renter

import (
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/HyperspaceApp/Hyperspace/build"
	"github.com/HyperspaceApp/Hyperspace/crypto"
	"github.com/HyperspaceApp/Hyperspace/modules"
	"github.com/HyperspaceApp/Hyperspace/modules/consensus"
	"github.com/HyperspaceApp/Hyperspace/modules/gateway"
	"github.com/HyperspaceApp/Hyperspace/modules/miner"
	"github.com/HyperspaceApp/Hyperspace/modules/renter/contractor"
	"github.com/HyperspaceApp/Hyperspace/modules/transactionpool"
	"github.com/HyperspaceApp/Hyperspace/modules/wallet"
	"github.com/HyperspaceApp/Hyperspace/types"
)

// renterTester contains all of the modules that are used while testing the renter.
type renterTester struct {
	cs        modules.ConsensusSet
	gateway   modules.Gateway
	miner     modules.TestMiner
	tpool     modules.TransactionPool
	wallet    modules.Wallet
	walletKey crypto.CipherKey

	renter *Renter
	dir    string
}

// Close shuts down the renter tester.
func (rt *renterTester) Close() error {
	rt.wallet.Lock()
	rt.cs.Close()
	rt.gateway.Close()
	return nil
}

// newRenterTester creates a ready-to-use renter tester with money in the
// wallet.
func newRenterTester(name string) (*renterTester, error) {
	// Create the modules.
	testdir := build.TempDir("renter", name)
	g, err := gateway.New("localhost:0", false, filepath.Join(testdir, modules.GatewayDir), false)
	if err != nil {
		return nil, err
	}
	cs, err := consensus.New(g, false, filepath.Join(testdir, modules.ConsensusDir), false)
	if err != nil {
		return nil, err
	}
	tp, err := transactionpool.New(cs, g, filepath.Join(testdir, modules.TransactionPoolDir))
	if err != nil {
		return nil, err
	}
	w, err := wallet.New(cs, tp, filepath.Join(testdir, modules.WalletDir), modules.DefaultAddressGapLimit, false)
	if err != nil {
		return nil, err
	}
	key := crypto.GenerateSiaKey(crypto.TypeDefaultWallet)
	_, err = w.Encrypt(key)
	if err != nil {
		return nil, err
	}
	err = w.Unlock(key)
	if err != nil {
		return nil, err
	}
	r, err := New(g, cs, w, tp, filepath.Join(testdir, modules.RenterDir))
	if err != nil {
		return nil, err
	}
	m, err := miner.New(cs, tp, w, filepath.Join(testdir, modules.MinerDir))
	if err != nil {
		return nil, err
	}

	// Assemble all pieces into a renter tester.
	rt := &renterTester{
		cs:      cs,
		gateway: g,
		miner:   m,
		tpool:   tp,
		wallet:  w,

		renter: r,
		dir:    testdir,
	}

	// Mine blocks until there is money in the wallet.
	for i := types.BlockHeight(0); i <= types.MaturityDelay; i++ {
		_, err := rt.miner.AddBlock()
		if err != nil {
			return nil, err
		}
	}
	return rt, nil
}

// stubHostDB is the minimal implementation of the hostDB interface. It can be
// embedded in other mock hostDB types, removing the need to re-implement all
// of the hostDB's methods on every mock.
type stubHostDB struct{}

func (stubHostDB) ActiveHosts() []modules.HostDBEntry   { return nil }
func (stubHostDB) AllHosts() []modules.HostDBEntry      { return nil }
func (stubHostDB) AverageContractPrice() types.Currency { return types.Currency{} }
func (stubHostDB) Close() error                         { return nil }
func (stubHostDB) IsOffline(modules.NetAddress) bool    { return true }
func (stubHostDB) RandomHosts(int, []types.SiaPublicKey) ([]modules.HostDBEntry, error) {
	return []modules.HostDBEntry{}, nil
}
func (stubHostDB) EstimateHostScore(modules.HostDBEntry, modules.Allowance) modules.HostScoreBreakdown {
	return modules.HostScoreBreakdown{}
}
func (stubHostDB) Host(types.SiaPublicKey) (modules.HostDBEntry, bool) {
	return modules.HostDBEntry{}, false
}
func (stubHostDB) ScoreBreakdown(modules.HostDBEntry) modules.HostScoreBreakdown {
	return modules.HostScoreBreakdown{}
}

// stubContractor is the minimal implementation of the hostContractor
// interface.
type stubContractor struct{}

func (stubContractor) SetAllowance(modules.Allowance) error { return nil }
func (stubContractor) Allowance() modules.Allowance         { return modules.Allowance{} }
func (stubContractor) Contract(modules.NetAddress) (modules.RenterContract, bool) {
	return modules.RenterContract{}, false
}
func (stubContractor) Contracts() []modules.RenterContract                    { return nil }
func (stubContractor) CurrentPeriod() types.BlockHeight                       { return 0 }
func (stubContractor) IsOffline(modules.NetAddress) bool                      { return false }
func (stubContractor) Editor(types.FileContractID) (contractor.Editor, error) { return nil, nil }
func (stubContractor) Downloader(types.FileContractID) (contractor.Downloader, error) {
	return nil, nil
}

type pricesStub struct {
	stubHostDB

	dbEntries []modules.HostDBEntry
}

func (pricesStub) InitialScanComplete() (bool, error) { return true, nil }

func (ps pricesStub) RandomHosts(_ int, _, _ []types.SiaPublicKey) ([]modules.HostDBEntry, error) {
	return ps.dbEntries, nil
}
func (ps pricesStub) RandomHostsWithAllowance(_ int, _, _ []types.SiaPublicKey, _ modules.Allowance) ([]modules.HostDBEntry, error) {
	return ps.dbEntries, nil
}

// TestRenterPricesVolatility verifies that the renter caches its price
// estimation, and subsequent calls result in non-volatile results.
func TestRenterPricesVolatility(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// create a stubbed hostdb, query it with one contract, add another, verify
	// the price estimation remains constant until the timeout has passed.
	hdb := &pricesStub{}
	id := rt.renter.mu.Lock()
	rt.renter.hostDB = hdb
	rt.renter.mu.Unlock(id)
	dbe := modules.HostDBEntry{}
	dbe.ContractPrice = types.SiacoinPrecision
	dbe.DownloadBandwidthPrice = types.SiacoinPrecision
	dbe.UploadBandwidthPrice = types.SiacoinPrecision
	dbe.StoragePrice = types.SiacoinPrecision
	// Add 4 host entries in the database with different public keys.
	for len(hdb.dbEntries) < modules.PriceEstimationScope {
		pk := []byte(strconv.Itoa(len(hdb.dbEntries)))
		dbe.PublicKey = types.SiaPublicKey{Key: pk}
		hdb.dbEntries = append(hdb.dbEntries, dbe)
	}
	allowance := modules.Allowance{}
	initial, _, err := rt.renter.PriceEstimation(allowance)
	if err != nil {
		t.Fatal(err)
	}
	// Changing the contract price should be enough to trigger a change
	// if the hosts are not cached.
	dbe.ContractPrice = dbe.ContractPrice.Mul64(2)
	pk := []byte(strconv.Itoa(len(hdb.dbEntries)))
	dbe.PublicKey = types.SiaPublicKey{Key: pk}
	hdb.dbEntries = append(hdb.dbEntries, dbe)
	after, _, err := rt.renter.PriceEstimation(allowance)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial, after) {
		t.Log(initial)
		t.Log(after)
		t.Fatal("expected renter price estimation to be constant")
	}
}

// TestRenterSiapathValidate verifies that the validateSiapath function correctly validates SiaPaths.
func TestRenterSiapathValidate(t *testing.T) {
	var pathtests = []struct {
		in    string
		valid bool
	}{
		{"valid/hyperspacepath", true},
		{"../../../directory/traversal", false},
		{"testpath", true},
		{"valid/hyperspacepath/../with/directory/traversal", false},
		{"validpath/test", true},
		{"..validpath/..test", true},
		{"./invalid/path", false},
		{".../path", true},
		{"valid./path", true},
		{"valid../path", true},
		{"valid/path./test", true},
		{"valid/path../test", true},
		{"test/path", true},
		{"/leading/slash", false},
		{"foo/./bar", false},
		{"", false},
		{"blank/end/", false},
	}
	for _, pathtest := range pathtests {
		err := validateSiapath(pathtest.in)
		if err != nil && pathtest.valid {
			t.Fatal("validateSiapath failed on valid path: ", pathtest.in)
		}
		if err == nil && !pathtest.valid {
			t.Fatal("validateSiapath succeeded on invalid path: ", pathtest.in)
		}
	}
}
