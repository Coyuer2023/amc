// Copyright 2022 The AmazeChain Authors
// This file is part of the AmazeChain library.
//
// The AmazeChain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The AmazeChain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the AmazeChain library. If not, see <http://www.gnu.org/licenses/>.

package apoa

import (
	"bytes"
	"encoding/json"
	"github.com/amazechain/amc/common/block"
	"github.com/amazechain/amc/common/types"
	"github.com/amazechain/amc/conf"
	"github.com/amazechain/amc/internal/avm/common"
	"github.com/amazechain/amc/log"
	"github.com/amazechain/amc/modules/rawdb"
	"github.com/ledgerwatch/erigon-lib/kv"
	"sort"
	"time"

	lru "github.com/hashicorp/golang-lru"
)

// Vote represents a single vote that an authorized signer made to modify the
// list of authorizations.
// Vote indicates detailed information about a vote
type Vote struct {
	Signer    types.Address `json:"signer"`    // Authorized signer that cast this vote
	Block     uint64        `json:"block"`     // Block number the vote was cast in (expire old votes)
	Address   types.Address `json:"address"`   // Account being voted on to change its authorization
	Authorize bool          `json:"authorize"` // Whether to authorize or deauthorize the voted account
}

// Tally is a simple vote tally to keep the current score of votes. Votes that
// go against the proposal aren't counted since it's equivalent to not voting.
// Tally the votes
type Tally struct {
	Authorize bool `json:"authorize"` // Whether the vote is about authorizing or kicking someone
	Votes     int  `json:"votes"`     // Number of votes until now wanting to pass the proposal
}

// Snapshot is the state of the authorization voting at a given point in time.
// Snapshot counts the voting information and signer list in a round. Every other period (1024 blocks),
// the data structure is saved on disk. When the corresponding Snapshot is used, it can be directly called from the disk
type Snapshot struct {
	config   *conf.APoaConfig // Consensus engine parameters to fine tune behavior
	sigcache *lru.ARCCache    // Cache of recent block signatures to speed up ecrecover

	Number  uint64                     `json:"number"`  // Block number where the snapshot was created
	Hash    types.Hash                 `json:"hash"`    // Block hash where the snapshot was created
	Signers map[types.Address]struct{} `json:"signers"` // Set of authorized signers at this moment
	Recents map[uint64]types.Address   `json:"recents"` // Set of recent signers for spam protections   The address of the signer of the most recent block
	Votes   []*Vote                    `json:"votes"`   // List of votes cast in chronological order
	Tally   map[types.Address]Tally    `json:"tally"`   // Current vote tally to avoid recalculating
}

// signersAscending implements the sort interface to allow sorting a list of addresses
type signersAscending []types.Address

func (s signersAscending) Len() int           { return len(s) }
func (s signersAscending) Less(i, j int) bool { return bytes.Compare(s[i][:], s[j][:]) < 0 }
func (s signersAscending) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// newSnapshot creates a new snapshot with the specified startup parameters. This
// method does not initialize the set of recent signers, so only ever use if for
// the genesis block.
func newSnapshot(config *conf.APoaConfig, sigcache *lru.ARCCache, number uint64, hash types.Hash, signers []types.Address) *Snapshot {
	snap := &Snapshot{
		config:   config,
		sigcache: sigcache,
		Number:   number,
		Hash:     hash,
		Signers:  make(map[types.Address]struct{}),
		Recents:  make(map[uint64]types.Address),
		Tally:    make(map[types.Address]Tally),
	}
	// Iterate over the list of Signers and store the signers address in the Signers map of the Snapshot object.
	// Use struct{} to assign a unique flag to the address for use in statistics.
	for _, signer := range signers {
		snap.Signers[signer] = struct{}{}
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *conf.APoaConfig, sigcache *lru.ARCCache, tx kv.Getter, hash types.Hash) (*Snapshot, error) {
	blob, err := rawdb.GetPoaSnapshot(tx, hash)
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config     // Configure the snap config property as a pointer to PoaConfig (POA configuration)
	snap.sigcache = sigcache // Configure the snap sigcache property as a pointer to the LRU cache

	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(tx kv.Putter) error { //The parameter is a pointer to Snapshot
	blob, err := json.Marshal(s) //Convert s (snapshot object) to JSON format and store it as a blob object
	if err != nil {
		return err
	}

	// Writes the blob object to the snapshot database, where s.ash is the hash of the snapshot object.
	// Returns nil if  succeeded, otherwise returns an error object
	return rawdb.StorePoaSnapshot(tx, s.Hash, blob)
}

// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{ // Example Create snapshot object cpy
		config:   s.config,
		sigcache: s.sigcache,
		Number:   s.Number,
		Hash:     s.Hash,
		Signers:  make(map[types.Address]struct{}),
		Recents:  make(map[uint64]types.Address),
		Votes:    make([]*Vote, len(s.Votes)),
		Tally:    make(map[types.Address]Tally),
	}
	// Iterate over the elements in s.Signers, s.Revents, and s.Tally and add them to the corresponding mapping of the new object cpy
	for signer := range s.Signers {
		cpy.Signers[signer] = struct{}{}
	}
	for block, signer := range s.Recents {
		cpy.Recents[block] = signer
	}
	for address, tally := range s.Tally {
		cpy.Tally[address] = tally
	}
	copy(cpy.Votes, s.Votes) // Copy the elements from s.Votes to cpy.Votes

	return cpy
}

// validVote returns whether it makes sense to cast the specified vote in the
// given snapshot context (e.g. don't try to add an already authorized signer).
func (s *Snapshot) validVote(address types.Address, authorize bool) bool {
	_, signer := s.Signers[address] // Gets the key-value pair equal to address (the signer value) in the s.Signers object
	return (signer && !authorize) || (!signer && authorize)
}

// cast adds a new vote into the tally.
func (s *Snapshot) cast(address types.Address, authorize bool) bool {
	// Ensure the vote is meaningful  Verify the voting address and authorization status
	if !s.validVote(address, authorize) {
		return false
	}
	// Cast the vote into an existing or new tally
	if old, ok := s.Tally[address]; ok {
		// Update the vote counter if the vote record already exists in the Tally
		old.Votes++
		s.Tally[address] = old
	} else {
		// Otherwise create a new vote count
		s.Tally[address] = Tally{Authorize: authorize, Votes: 1}
	}
	return true
}

// uncast removes a previously cast vote from the tally.
func (s *Snapshot) uncast(address types.Address, authorize bool) bool {
	// If there's no tally, it's a dangling vote, just drop
	tally, ok := s.Tally[address]
	if !ok {
		return false
	}
	// Ensure we only revert counted votes
	if tally.Authorize != authorize {
		return false
	}
	// Otherwise revert the vote
	if tally.Votes > 1 {
		tally.Votes--
		s.Tally[address] = tally
	} else {
		delete(s.Tally, address)
	}
	return true
}

// apply creates a new authorization snapshot by applying the given headers to
// the original one.
// apply takes the block headers as input, counts all voting information for those block headers,
// and finally updates the output of the current snapshot object
func (s *Snapshot) apply(headers []block.IHeader) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].(*block.Header).Number.Uint64() != headers[i].(*block.Header).Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}
	if headers[0].(*block.Header).Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	var (
		start  = time.Now()
		logged = time.Now()
	)
	for i, iHeader := range headers {
		header := iHeader.(*block.Header)
		// Remove any votes on checkpoint blocks
		number := header.Number.Uint64()
		if number%s.config.Epoch == 0 {
			snap.Votes = nil
			snap.Tally = make(map[types.Address]Tally)
		}
		// Delete the oldest signer from the recent list to allow it signing again
		if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
			delete(snap.Recents, number-limit)
		}
		// Resolve the authorization key and check against signers
		signer, err := ecrecover(header, s.sigcache)
		if err != nil {
			return nil, err
		}
		if _, ok := snap.Signers[signer]; !ok {
			return nil, errUnauthorizedSigner
		}
		for _, recent := range snap.Recents {
			if recent == signer {
				return nil, errRecentlySigned
			}
		}
		snap.Recents[number] = signer

		// Header authorized, discard any previous votes from the signer
		// Ensure that a signer within an epoch can vote only once
		for i, vote := range snap.Votes {
			if vote.Signer == signer && vote.Address == header.Coinbase {
				// Uncast the vote from the cached tally
				snap.uncast(vote.Address, vote.Authorize)

				// Uncast the vote from the chronological list
				snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
				break // only one vote allowed
			}
		}
		// Tally up the new vote from the signer
		var authorize bool
		switch {
		case bytes.Equal(header.Nonce[:], nonceAuthVote):
			authorize = true
		case bytes.Equal(header.Nonce[:], nonceDropVote):
			authorize = false
		default:
			return nil, errInvalidVote
		}
		if snap.cast(header.Coinbase, authorize) {
			snap.Votes = append(snap.Votes, &Vote{
				Signer:    signer,
				Block:     number,
				Address:   header.Coinbase,
				Authorize: authorize,
			})
		}
		// If the vote passed, update the list of signers
		// With more than half the votes cast, the vote passed
		if tally := snap.Tally[header.Coinbase]; tally.Votes > len(snap.Signers)/2 {
			if tally.Authorize { // If it is a join vote, it will be added to the list of Signers by the voter, and the block can be produced later
				snap.Signers[header.Coinbase] = struct{}{}
			} else {
				delete(snap.Signers, header.Coinbase) // If the vote is excluded, the voted person will be removed from the list of Signers and cannot participate in the block after that

				// Signer list shrunk, delete any leftover recent caches
				if limit := uint64(len(snap.Signers)/2 + 1); number >= limit {
					delete(snap.Recents, number-limit)
				}
				// Discard any previous votes the deauthorized signer cast
				for i := 0; i < len(snap.Votes); i++ {
					if snap.Votes[i].Signer == header.Coinbase {
						// Uncast the vote from the cached tally
						snap.uncast(snap.Votes[i].Address, snap.Votes[i].Authorize)

						// Uncast the vote from the chronological list
						snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)

						i--
					}
				}
			}
			// Discard any previous votes around the just changed account
			for i := 0; i < len(snap.Votes); i++ {
				if snap.Votes[i].Address == header.Coinbase {
					snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
					i--
				}
			}
			delete(snap.Tally, header.Coinbase) // Clear the current vote count
		}
		// If we're taking too much time (ecrecover), notify the user once a while
		if time.Since(logged) > 8*time.Second {
			log.Info("Reconstructing voting history", "processed", i, "total", len(headers), "elapsed", common.PrettyDuration(time.Since(start)))
			logged = time.Now()
		}
	}
	if time.Since(start) > 8*time.Second {
		//log.Info("Reconstructed voting history", "processed", len(headers), "elapsed", common.PrettyDuration(time.Since(start)))
	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()

	return snap, nil
}

// signers retrieves the list of authorized signers in ascending order.
func (s *Snapshot) signers() []types.Address {
	sigs := make([]types.Address, 0, len(s.Signers))
	for sig := range s.Signers {
		sigs = append(sigs, sig)
	}
	sort.Sort(signersAscending(sigs))
	return sigs
}

// inturn returns if a signer at a given block height is in-turn or not.
// intern by determining whether the height of the current block is in the same order as it is in the signer list
func (s *Snapshot) inturn(number uint64, signer types.Address) bool {
	signers, offset := s.signers(), 0
	for offset < len(signers) && signers[offset] != signer {
		offset++
	}
	return (number % uint64(len(signers))) == uint64(offset)
}
