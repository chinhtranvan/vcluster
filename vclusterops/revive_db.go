/*
 (c) Copyright [2023] Open Text.
 Licensed under the Apache License, Version 2.0 (the "License");
 You may not use this file except in compliance with the License.
 You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package vclusterops

import (
	"fmt"
	"sort"

	"github.com/vertica/vcluster/vclusterops/util"
)

type VReviveDatabaseOptions struct {
	/* part 1: basic db info */
	DatabaseOptions

	/* part 2: revive db info */

	// timeout in seconds of loading remote catalog
	LoadCatalogTimeout *uint
	// whether force remove existing directories before revive the database
	ForceRemoval *bool
	// describe the database on communal storage, and exit
	DisplayOnly *bool
	// whether ignore the cluster lease
	IgnoreClusterLease *bool
	// the restore policy
	RestorePoint *RestorePointPolicy
}

type RestorePointPolicy struct {
	// Name of the restore archive to use for bootstrapping
	Archive *string
	// The (1-based) index of the restore point in the restore archive to restore from
	Index *int
	// The identifier of the restore point in the restore archive to restore from
	ID *string
}

func (options *VReviveDatabaseOptions) isRestoreEnabled() bool {
	return options.RestorePoint != nil && options.RestorePoint.Archive != nil && *options.RestorePoint.Archive != ""
}

func (options *VReviveDatabaseOptions) hasValidRestorePointID() bool {
	return options.RestorePoint != nil && options.RestorePoint.ID != nil && *options.RestorePoint.ID != ""
}

func (options *VReviveDatabaseOptions) hasValidRestorePointIndex() bool {
	return options.RestorePoint != nil && options.RestorePoint.Index != nil && *options.RestorePoint.Index > 0
}

func (options *VReviveDatabaseOptions) findSpecifiedRestorePoint(allRestorePoints []RestorePoint) (string, error) {
	foundRestorePoints := make([]RestorePoint, 0)
	for _, restorePoint := range allRestorePoints {
		if restorePoint.Archive != *options.RestorePoint.Archive {
			continue
		}
		if restorePoint.ID == *options.RestorePoint.ID || restorePoint.Index == *options.RestorePoint.Index {
			foundRestorePoints = append(foundRestorePoints, restorePoint)
		}
	}
	if len(foundRestorePoints) == 0 {
		err := &ReviveDBRestorePointNotFoundError{Archive: *options.RestorePoint.Archive}
		if options.hasValidRestorePointID() {
			err.InvalidID = *options.RestorePoint.ID
		} else {
			err.InvalidIndex = *options.RestorePoint.Index
		}
		return "", err
	}
	if len(foundRestorePoints) == 1 {
		return foundRestorePoints[0].ID, nil // #nosec G602
	}
	return "", fmt.Errorf("found %d restore points instead of 1: %+v", len(foundRestorePoints), foundRestorePoints)
}

// ReviveDBRestorePointNotFoundError is the error that is returned when the retore point specified by the user
// either via index or id is not found among all restore points in the specified archive. Either InvalidID or
// InvalidIndex will be set depending on whether the user specified the retore point by index or id.
type ReviveDBRestorePointNotFoundError struct {
	Archive      string
	InvalidID    string
	InvalidIndex int
}

func (e *ReviveDBRestorePointNotFoundError) Error() string {
	var indicator, value string
	if e.InvalidID != "" {
		indicator = "ID"
		value = e.InvalidID
	} else {
		indicator = "index"
		value = fmt.Sprintf("%d", e.InvalidIndex)
	}
	return fmt.Sprintf("restore point with %s %s not found in archive %q", indicator, value, e.Archive)
}

func VReviveDBOptionsFactory() VReviveDatabaseOptions {
	opt := VReviveDatabaseOptions{}

	// set default values to the params
	opt.setDefaultValues()

	return opt
}

func (options *VReviveDatabaseOptions) setDefaultValues() {
	options.DatabaseOptions.setDefaultValues()

	// set default values for revive db options
	options.LoadCatalogTimeout = new(uint)
	*options.LoadCatalogTimeout = util.DefaultLoadCatalogTimeoutSeconds
	options.ForceRemoval = new(bool)
	options.DisplayOnly = new(bool)
	options.IgnoreClusterLease = new(bool)
	options.RestorePoint = new(RestorePointPolicy)
	options.RestorePoint.Archive = new(string)
	options.RestorePoint.Index = new(int)
	options.RestorePoint.ID = new(string)
}

func (options *VReviveDatabaseOptions) validateRequiredOptions() error {
	// database name
	if *options.DBName == "" {
		return fmt.Errorf("must specify a database name")
	}
	err := util.ValidateName(*options.DBName, "database")
	if err != nil {
		return err
	}

	// new hosts
	// when --display-only is not specified, we require --hosts
	if len(options.RawHosts) == 0 && !*options.DisplayOnly {
		return fmt.Errorf("must specify a host or host list")
	}

	// communal storage
	return util.ValidateCommunalStorageLocation(*options.CommunalStorageLocation)
}

func (options *VReviveDatabaseOptions) validateRestoreOptions() error {
	if options.isRestoreEnabled() &&
		options.hasValidRestorePointID() == options.hasValidRestorePointIndex() {
		return fmt.Errorf("for a restore, must specify exactly one of (1-based) restore point index or id, " +
			"not both or none")
	}

	return nil
}

func (options *VReviveDatabaseOptions) validateParseOptions() error {
	err := options.validateRequiredOptions()
	if err != nil {
		return err
	}

	return options.validateRestoreOptions()
}

// analyzeOptions will modify some options based on what is chosen
func (options *VReviveDatabaseOptions) analyzeOptions() (err error) {
	// when --display-only is specified but no hosts in user input, we will try to access communal storage from localhost
	if len(options.RawHosts) == 0 && *options.DisplayOnly {
		options.RawHosts = append(options.RawHosts, "localhost")
	}

	// resolve RawHosts to be IP addresses
	options.Hosts, err = util.ResolveRawHostsToAddresses(options.RawHosts, options.Ipv6.ToBool())
	if err != nil {
		return err
	}

	return nil
}

func (options *VReviveDatabaseOptions) validateAnalyzeOptions() error {
	if err := options.validateParseOptions(); err != nil {
		return err
	}
	return options.analyzeOptions()
}

// VReviveDatabase revives a database that was terminated but whose communal storage data still exists.
// It returns the database information retrieved from communal storage and any error encountered.
func (vcc *VClusterCommands) VReviveDatabase(options *VReviveDatabaseOptions) (dbInfo string, vdbPtr *VCoordinationDatabase, err error) {
	/*
	 *   - Validate options
	 *   - Run VClusterOpEngine to get terminated database info
	 *   - Run VClusterOpEngine again to revive the database
	 */

	// validate and analyze options
	err = options.validateAnalyzeOptions()
	if err != nil {
		return dbInfo, nil, err
	}

	vdb := makeVCoordinationDatabase()

	// part 1: produce instructions for getting terminated database info, and save the info to vdb
	preReviveDBInstructions, err := vcc.producePreReviveDBInstructions(options, &vdb)
	if err != nil {
		return dbInfo, nil, fmt.Errorf("fail to produce pre-revive database instructions %w", err)
	}

	// generate clusterOpEngine certs
	certs := httpsCerts{key: options.Key, cert: options.Cert, caCert: options.CaCert}
	// feed the pre-revive db instructions to the VClusterOpEngine
	clusterOpEngine := makeClusterOpEngine(preReviveDBInstructions, &certs)
	err = clusterOpEngine.run(vcc.Log)
	if err != nil {
		return dbInfo, nil, fmt.Errorf("fail to collect the information of database in revive_db %w", err)
	}

	if options.isRestoreEnabled() {
		validatedRestorePointID, findErr := options.findSpecifiedRestorePoint(clusterOpEngine.execContext.restorePoints)
		if findErr != nil {
			return dbInfo, &vdb, fmt.Errorf("fail to find a restore point as specified %w", findErr)
		}

		restoreDBSpecificInstructions, produceErr := vcc.produceRestoreDBSpecificInstructions(options, &vdb, validatedRestorePointID)
		if produceErr != nil {
			return dbInfo, &vdb, fmt.Errorf("fail to produce restore-specific instructions %w", produceErr)
		}

		// feed the restore db specific instructions to the VClusterOpEngine
		clusterOpEngine = makeClusterOpEngine(restoreDBSpecificInstructions, &certs)
		runErr := clusterOpEngine.run(vcc.Log)
		if runErr != nil {
			return dbInfo, &vdb, fmt.Errorf("fail to collect the restore-specific information of database in revive_db %w", runErr)
		}
	}

	if *options.DisplayOnly {
		dbInfo = clusterOpEngine.execContext.dbInfo
		return dbInfo, &vdb, nil
	}

	// part 2: produce instructions for reviving database using terminated database info
	reviveDBInstructions, err := vcc.produceReviveDBInstructions(options, &vdb)
	if err != nil {
		return dbInfo, &vdb, fmt.Errorf("fail to produce revive database instructions %w", err)
	}

	// feed revive db instructions to the VClusterOpEngine
	clusterOpEngine = makeClusterOpEngine(reviveDBInstructions, &certs)
	err = clusterOpEngine.run(vcc.Log)
	if err != nil {
		return dbInfo, &vdb, fmt.Errorf("fail to revive database %w", err)
	}

	// fill vdb with VReviveDatabaseOptions information
	vdb.Name = *options.DBName
	vdb.IsEon = true
	vdb.CommunalStorageLocation = *options.CommunalStorageLocation
	vdb.Ipv6 = options.Ipv6.ToBool()

	return dbInfo, &vdb, nil
}

// revive db instructions are split into two parts:
// 1. get terminated database info
// 2. revive database using the info we got from step 1
// The reason of using two set of instructions is: the second set of instructions needs the database info
// to initialize, but that info can only be retrieved after we ran first set of instructions in clusterOpEngine
//
// producePreReviveDBInstructions will build the majority of first half of revive_db instructions
// The generated instructions will later perform the following operations
//   - Check NMA connectivity
//   - Check any DB running on the hosts
//   - (Optionally) download and read the current description file from communal storage on the initiator
//   - (Optionally) list all restore points
func (vcc *VClusterCommands) producePreReviveDBInstructions(options *VReviveDatabaseOptions,
	vdb *VCoordinationDatabase) ([]clusterOp, error) {
	var instructions []clusterOp

	nmaHealthOp := makeNMAHealthOp(vcc.Log, options.Hosts)

	checkDBRunningOp, err := makeHTTPSCheckRunningDBOp(vcc.Log, options.Hosts, false, /*use password auth*/
		"" /*username for https call*/, nil /*password for https call*/, ReviveDB)
	if err != nil {
		return instructions, err
	}
	instructions = append(instructions,
		&nmaHealthOp,
		&checkDBRunningOp,
	)

	// use current description file path as source file path
	currConfigFileSrcPath := options.getCurrConfigFilePath()

	if !options.isRestoreEnabled() {
		// perform revive, either display-only or not
		nmaDownloadFileOpForRevive, err := makeNMADownloadFileOpForRevive(vcc.Log, options.Hosts,
			currConfigFileSrcPath, currConfigFileDestPath, catalogPath,
			options.ConfigurationParameters, vdb, *options.DisplayOnly, *options.IgnoreClusterLease)
		if err != nil {
			return instructions, err
		}
		instructions = append(instructions,
			&nmaDownloadFileOpForRevive,
		)
	} else {
		// perform restore
		if !*options.DisplayOnly {
			// if not display-only, do a lease check first using current cluster config
			nmaDownloadFileOpForRestoreLeaseCheck, err := makeNMADownloadFileOpForRestoreLeaseCheck(vcc.Log, options.Hosts,
				currConfigFileSrcPath, currConfigFileDestPath, catalogPath,
				options.ConfigurationParameters, vdb, *options.IgnoreClusterLease)
			if err != nil {
				return instructions, err
			}
			instructions = append(instructions,
				&nmaDownloadFileOpForRestoreLeaseCheck,
			)
		}
		// no matter display-only or not, list all restore points for later use
		hosts := options.Hosts
		initiator := getInitiator(hosts)
		bootstrapHost := []string{initiator}
		nmaShowRestorePointsOp := makeNMAShowRestorePointsOp(vcc.Log, bootstrapHost, *options.DBName,
			*options.CommunalStorageLocation, options.ConfigurationParameters)
		instructions = append(instructions,
			&nmaShowRestorePointsOp,
		)
	}

	return instructions, nil
}

// produceRestoreDBSpecificInstructions will complete building the first half of revive_db instructions when a restore is enabled
// The generated instructions will later perform the following operations
//   - Download and read the description file corresponding to the restore point from communal storage on the initiator
func (vcc *VClusterCommands) produceRestoreDBSpecificInstructions(options *VReviveDatabaseOptions,
	vdb *VCoordinationDatabase, validatedRestorePointID string) ([]clusterOp, error) {
	var instructions []clusterOp

	restorePointConfigFileSrcPath := options.getRestorePointConfigFilePath(validatedRestorePointID)

	nmaDownLoadFileOp, err := makeNMADownloadFileOpForRestore(vcc.Log, options.Hosts,
		restorePointConfigFileSrcPath, restorePointConfigFileDestPath, catalogPath,
		options.ConfigurationParameters, vdb, *options.DisplayOnly)

	if err != nil {
		return instructions, err
	}

	instructions = append(instructions,
		&nmaDownLoadFileOp,
	)

	return instructions, nil
}

// produceReviveDBInstructions will build the second half of revive_db instructions
// The generated instructions will later perform the following operations
//   - Prepare database directories for all the hosts
//   - Get network profiles for all the hosts
//   - Load remote catalog from communal storage on all the hosts
func (vcc *VClusterCommands) produceReviveDBInstructions(options *VReviveDatabaseOptions, vdb *VCoordinationDatabase) ([]clusterOp, error) {
	var instructions []clusterOp

	newVDB, oldHosts, err := options.generateReviveVDB(vdb)
	if err != nil {
		return instructions, err
	}

	// create a new HostNodeMap to prepare directories
	hostNodeMap := makeVHostNodeMap()
	// remove user storage locations from storage locations in every node
	// user storage location will not be force deleted,
	// and fail to create user storage location will not cause a failure of NMA /directories/prepare call.
	// as a result, we separate user storage locations with other storage locations
	for host, vnode := range newVDB.HostNodeMap {
		userLocationSet := make(map[string]struct{})
		for _, userLocation := range vnode.UserStorageLocations {
			userLocationSet[userLocation] = struct{}{}
		}
		var newLocations []string
		for _, location := range vnode.StorageLocations {
			if _, exist := userLocationSet[location]; !exist {
				newLocations = append(newLocations, location)
			}
		}
		vnode.StorageLocations = newLocations
		hostNodeMap[host] = vnode
	}
	// prepare all directories
	nmaPrepareDirectoriesOp, err := makeNMAPrepareDirectoriesOp(vcc.Log, hostNodeMap, *options.ForceRemoval, true /*for db revive*/)
	if err != nil {
		return instructions, err
	}

	nmaNetworkProfileOp := makeNMANetworkProfileOp(vcc.Log, options.Hosts)

	nmaLoadRemoteCatalogOp := makeNMALoadRemoteCatalogOp(vcc.Log, oldHosts, options.ConfigurationParameters,
		&newVDB, *options.LoadCatalogTimeout, options.RestorePoint)

	instructions = append(instructions,
		&nmaPrepareDirectoriesOp,
		&nmaNetworkProfileOp,
		&nmaLoadRemoteCatalogOp,
	)

	return instructions, nil
}

// generateReviveVDB can create new vdb, and line up old hosts and vnodes with new hosts' order(user input order)
func (options *VReviveDatabaseOptions) generateReviveVDB(vdb *VCoordinationDatabase) (newVDB VCoordinationDatabase,
	oldHosts []string, err error) {
	newVDB = makeVCoordinationDatabase()
	newVDB.Name = *options.DBName
	newVDB.CommunalStorageLocation = *options.CommunalStorageLocation
	// use new cluster hosts
	newVDB.HostList = options.Hosts

	/* for example, in old vdb, we could have the HostNodeMap
	{
	"192.168.1.101": {Name: v_test_db_node0001, Address: "192.168.1.101", ...},
	"192.168.1.102": {Name: v_test_db_node0002, Address: "192.168.1.102", ...},
	"192.168.1.103": {Name: v_test_db_node0003, Address: "192.168.1.103", ...}
	}
	in new vdb, we want to update the HostNodeMap with the values(can be unordered) in --hosts(user input), e.g. 10.1.10.2,10.1.10.1,10.1.10.3.
	we line up vnodes with new hosts' order(user input order). We will have the new HostNodeMap like:
	{
	"10.1.10.2": {Name: v_test_db_node0001, Address: "10.1.10.2", ...},
	"10.1.10.1": {Name: v_test_db_node0002, Address: "10.1.10.1", ...},
	"10.1.10.3": {Name: v_test_db_node0003, Address: "10.1.10.3", ...}
	}
	we also line up old nodes with new hosts' order so we will have oldHosts like:
	["192.168.1.102", "192.168.1.101", "192.168.1.103"]
	*/
	// sort nodes by their names, and then assign new hosts to them
	var vNodes []*VCoordinationNode
	for _, vnode := range vdb.HostNodeMap {
		vNodes = append(vNodes, vnode)
	}
	sort.Slice(vNodes, func(i, j int) bool {
		return vNodes[i].Name < vNodes[j].Name
	})

	newVDB.HostNodeMap = makeVHostNodeMap()
	if len(newVDB.HostList) != len(vNodes) {
		return newVDB, oldHosts, fmt.Errorf("the number of new hosts does not match the number of nodes in original database")
	}
	for index, newHost := range newVDB.HostList {
		// recreate the old host list with new hosts' order
		oldHosts = append(oldHosts, vNodes[index].Address)
		vNodes[index].Address = newHost
		newVDB.HostNodeMap[newHost] = vNodes[index]
	}

	return newVDB, oldHosts, nil
}
