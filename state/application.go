// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package state

import (
	stderrors "errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/juju/errors"
	jujutxn "github.com/juju/txn"
	"github.com/juju/utils/featureflag"
	"github.com/juju/utils/series"
	"gopkg.in/juju/charm.v6-unstable"
	csparams "gopkg.in/juju/charmrepo.v2-unstable/csclient/params"
	"gopkg.in/juju/names.v2"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/constraints"
	"github.com/juju/juju/core/leadership"
	"github.com/juju/juju/feature"
	"github.com/juju/juju/status"
)

// Application represents the state of an application.
type Application struct {
	st  *State
	doc applicationDoc
}

// applicationDoc represents the internal state of an application in MongoDB.
// Note the correspondence with ApplicationInfo in apiserver.
type applicationDoc struct {
	DocID                string     `bson:"_id"`
	Name                 string     `bson:"name"`
	ModelUUID            string     `bson:"model-uuid"`
	Series               string     `bson:"series"`
	Subordinate          bool       `bson:"subordinate"`
	CharmURL             *charm.URL `bson:"charmurl"`
	Channel              string     `bson:"cs-channel"`
	CharmModifiedVersion int        `bson:"charmmodifiedversion"`
	ForceCharm           bool       `bson:"forcecharm"`
	Life                 Life       `bson:"life"`
	UnitCount            int        `bson:"unitcount"`
	RelationCount        int        `bson:"relationcount"`
	Exposed              bool       `bson:"exposed"`
	MinUnits             int        `bson:"minunits"`
	TxnRevno             int64      `bson:"txn-revno"`
	MetricCredentials    []byte     `bson:"metric-credentials"`
}

func newApplication(st *State, doc *applicationDoc) *Application {
	app := &Application{
		st:  st,
		doc: *doc,
	}
	return app
}

// IsRemote returns false for a local application.
func (a *Application) IsRemote() bool {
	return false
}

// Name returns the application name.
func (a *Application) Name() string {
	return a.doc.Name
}

// Tag returns a name identifying the application.
// The returned name will be different from other Tag values returned by any
// other entities from the same state.
func (a *Application) Tag() names.Tag {
	return a.ApplicationTag()
}

// ApplicationTag returns the more specific ApplicationTag rather than the generic
// Tag.
func (a *Application) ApplicationTag() names.ApplicationTag {
	return names.NewApplicationTag(a.Name())
}

// applicationGlobalKey returns the global database key for the application
// with the given name.
func applicationGlobalKey(svcName string) string {
	return "a#" + svcName
}

// globalKey returns the global database key for the application.
func (a *Application) globalKey() string {
	return applicationGlobalKey(a.doc.Name)
}

func applicationSettingsKey(appName string, curl *charm.URL) string {
	return fmt.Sprintf("a#%s#%s", appName, curl)
}

// settingsKey returns the charm-version-specific settings collection
// key for the application.
func (a *Application) settingsKey() string {
	return applicationSettingsKey(a.doc.Name, a.doc.CharmURL)
}

func applicationStorageConstraintsKey(appName string, curl *charm.URL) string {
	return fmt.Sprintf("asc#%s#%s", appName, curl)
}

// storageConstraintsKey returns the charm-version-specific storage
// constraints collection key for the application.
func (a *Application) storageConstraintsKey() string {
	return applicationStorageConstraintsKey(a.doc.Name, a.doc.CharmURL)
}

// Series returns the specified series for this charm.
func (a *Application) Series() string {
	return a.doc.Series
}

// Life returns whether the application is Alive, Dying or Dead.
func (a *Application) Life() Life {
	return a.doc.Life
}

var errRefresh = stderrors.New("state seems inconsistent, refresh and try again")

// Destroy ensures that the application and all its relations will be removed at
// some point; if the application has no units, and no relation involving the
// application has any units in scope, they are all removed immediately.
func (a *Application) Destroy() (err error) {
	defer errors.DeferredAnnotatef(&err, "cannot destroy application %q", a)
	defer func() {
		if err == nil {
			// This is a white lie; the document might actually be removed.
			a.doc.Life = Dying
		}
	}()
	app := &Application{st: a.st, doc: a.doc}
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			if err := app.Refresh(); errors.IsNotFound(err) {
				return nil, jujutxn.ErrNoOperations
			} else if err != nil {
				return nil, err
			}
		}
		switch ops, err := app.destroyOps(); err {
		case errRefresh:
		case errAlreadyDying:
			return nil, jujutxn.ErrNoOperations
		case nil:
			return ops, nil
		default:
			return nil, err
		}
		return nil, jujutxn.ErrTransientFailure
	}
	return a.st.run(buildTxn)
}

// destroyOps returns the operations required to destroy the application. If it
// returns errRefresh, the application should be refreshed and the destruction
// operations recalculated.
func (a *Application) destroyOps() ([]txn.Op, error) {
	if a.doc.Life == Dying {
		return nil, errAlreadyDying
	}
	rels, err := a.Relations()
	if err != nil {
		return nil, err
	}
	if len(rels) != a.doc.RelationCount {
		// This is just an early bail out. The relations obtained may still
		// be wrong, but that situation will be caught by a combination of
		// asserts on relationcount and on each known relation, below.
		return nil, errRefresh
	}
	ops := []txn.Op{minUnitsRemoveOp(a.st, a.doc.Name)}
	removeCount := 0
	for _, rel := range rels {
		relOps, isRemove, err := rel.destroyOps(a.doc.Name)
		if err == errAlreadyDying {
			relOps = []txn.Op{{
				C:      relationsC,
				Id:     rel.doc.DocID,
				Assert: bson.D{{"life", Dying}},
			}}
		} else if err != nil {
			return nil, err
		}
		if isRemove {
			removeCount++
		}
		ops = append(ops, relOps...)
	}
	// TODO(ericsnow) Use a generic registry instead.
	resOps, err := removeResourcesOps(a.st, a.doc.Name)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops = append(ops, resOps...)
	// If the application has no units, and all its known relations will be
	// removed, the application can also be removed.
	if a.doc.UnitCount == 0 && a.doc.RelationCount == removeCount {
		hasLastRefs := bson.D{{"life", Alive}, {"unitcount", 0}, {"relationcount", removeCount}}
		removeOps, err := a.removeOps(hasLastRefs)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return append(ops, removeOps...), nil
	}
	// In all other cases, application removal will be handled as a consequence
	// of the removal of the last unit or relation referencing it. If any
	// relations have been removed, they'll be caught by the operations
	// collected above; but if any has been added, we need to abort and add
	// a destroy op for that relation too. In combination, it's enough to
	// check for count equality: an add/remove will not touch the count, but
	// will be caught by virtue of being a remove.
	notLastRefs := bson.D{
		{"life", Alive},
		{"relationcount", a.doc.RelationCount},
	}
	// With respect to unit count, a changing value doesn't matter, so long
	// as the count's equality with zero does not change, because all we care
	// about is that *some* unit is, or is not, keeping the application from
	// being removed: the difference between 1 unit and 1000 is irrelevant.
	if a.doc.UnitCount > 0 {
		ops = append(ops, newCleanupOp(cleanupUnitsForDyingApplication, a.doc.Name))
		notLastRefs = append(notLastRefs, bson.D{{"unitcount", bson.D{{"$gt", 0}}}}...)
	} else {
		notLastRefs = append(notLastRefs, bson.D{{"unitcount", 0}}...)
	}
	update := bson.D{{"$set", bson.D{{"life", Dying}}}}
	if removeCount != 0 {
		decref := bson.D{{"$inc", bson.D{{"relationcount", -removeCount}}}}
		update = append(update, decref...)
	}
	return append(ops, txn.Op{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Assert: notLastRefs,
		Update: update,
	}), nil
}

func removeResourcesOps(st *State, applicationID string) ([]txn.Op, error) {
	persist, err := st.ResourcesPersistence()
	if errors.IsNotSupported(err) {
		// Nothing to see here, move along.
		return nil, nil
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops, err := persist.NewRemoveResourcesOps(applicationID)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return ops, nil
}

// removeOps returns the operations required to remove the application. Supplied
// asserts will be included in the operation on the application document.
func (a *Application) removeOps(asserts bson.D) ([]txn.Op, error) {
	ops := []txn.Op{
		{
			C:      applicationsC,
			Id:     a.doc.DocID,
			Assert: asserts,
			Remove: true,
		}, {
			C:      settingsC,
			Id:     a.settingsKey(),
			Remove: true,
		},
	}
	// Note that appCharmDecRefOps might not catch the final decref
	// when run in a transaction that decrefs more than once. In
	// this case, luckily, we can be sure that we unconditionally
	// need finalAppCharmRemoveOps; and we trust that it's written
	// such that it's safe to run multiple times.
	name := a.doc.Name
	curl := a.doc.CharmURL
	charmOps, err := appCharmDecRefOps(a.st, name, curl)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops = append(ops, charmOps...)
	ops = append(ops, finalAppCharmRemoveOps(name, curl)...)

	globalKey := a.globalKey()
	ops = append(ops,
		removeEndpointBindingsOp(globalKey),
		removeConstraintsOp(a.st, globalKey),
		annotationRemoveOp(a.st, globalKey),
		removeLeadershipSettingsOp(name),
		removeStatusOp(a.st, globalKey),
		removeModelApplicationRefOp(a.st, name),
	)
	return ops, nil
}

// IsExposed returns whether this application is exposed. The explicitly open
// ports (with open-port) for exposed applications may be accessed from machines
// outside of the local deployment network. See SetExposed and ClearExposed.
func (a *Application) IsExposed() bool {
	return a.doc.Exposed
}

// SetExposed marks the application as exposed.
// See ClearExposed and IsExposed.
func (a *Application) SetExposed() error {
	return a.setExposed(true)
}

// ClearExposed removes the exposed flag from the application.
// See SetExposed and IsExposed.
func (a *Application) ClearExposed() error {
	return a.setExposed(false)
}

func (a *Application) setExposed(exposed bool) (err error) {
	ops := []txn.Op{{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Assert: isAliveDoc,
		Update: bson.D{{"$set", bson.D{{"exposed", exposed}}}},
	}}
	if err := a.st.runTransaction(ops); err != nil {
		return errors.Errorf("cannot set exposed flag for application %q to %v: %v", a, exposed, onAbort(err, errNotAlive))
	}
	a.doc.Exposed = exposed
	return nil
}

// Charm returns the application's charm and whether units should upgrade to that
// charm even if they are in an error state.
func (a *Application) Charm() (ch *Charm, force bool, err error) {
	// We don't worry about the channel since we aren't interacting
	// with the charm store here.
	ch, err = a.st.Charm(a.doc.CharmURL)
	if err != nil {
		return nil, false, err
	}
	return ch, a.doc.ForceCharm, nil
}

// IsPrincipal returns whether units of the application can
// have subordinate units.
func (a *Application) IsPrincipal() bool {
	return !a.doc.Subordinate
}

// CharmModifiedVersion increases whenever the application's charm is changed in any
// way.
func (a *Application) CharmModifiedVersion() int {
	return a.doc.CharmModifiedVersion
}

// CharmURL returns the application's charm URL, and whether units should upgrade
// to the charm with that URL even if they are in an error state.
func (a *Application) CharmURL() (curl *charm.URL, force bool) {
	return a.doc.CharmURL, a.doc.ForceCharm
}

// Channel identifies the charm store channel from which the application's
// charm was deployed. It is only needed when interacting with the charm
// store.
func (a *Application) Channel() csparams.Channel {
	return csparams.Channel(a.doc.Channel)
}

// Endpoints returns the application's currently available relation endpoints.
func (a *Application) Endpoints() (eps []Endpoint, err error) {
	ch, _, err := a.Charm()
	if err != nil {
		return nil, err
	}
	collect := func(role charm.RelationRole, rels map[string]charm.Relation) {
		for _, rel := range rels {
			eps = append(eps, Endpoint{
				ApplicationName: a.doc.Name,
				Relation:        rel,
			})
		}
	}
	meta := ch.Meta()
	collect(charm.RolePeer, meta.Peers)
	collect(charm.RoleProvider, meta.Provides)
	collect(charm.RoleRequirer, meta.Requires)
	collect(charm.RoleProvider, map[string]charm.Relation{
		"juju-info": {
			Name:      "juju-info",
			Role:      charm.RoleProvider,
			Interface: "juju-info",
			Scope:     charm.ScopeGlobal,
		},
	})
	sort.Sort(epSlice(eps))
	return eps, nil
}

// Endpoint returns the relation endpoint with the supplied name, if it exists.
func (a *Application) Endpoint(relationName string) (Endpoint, error) {
	eps, err := a.Endpoints()
	if err != nil {
		return Endpoint{}, err
	}
	for _, ep := range eps {
		if ep.Name == relationName {
			return ep, nil
		}
	}
	return Endpoint{}, errors.Errorf("application %q has no %q relation", a, relationName)
}

// extraPeerRelations returns only the peer relations in newMeta not
// present in the application's current charm meta data.
func (a *Application) extraPeerRelations(newMeta *charm.Meta) map[string]charm.Relation {
	if newMeta == nil {
		// This should never happen, since we're checking the charm in SetCharm already.
		panic("newMeta is nil")
	}
	ch, _, err := a.Charm()
	if err != nil {
		return nil
	}
	newPeers := newMeta.Peers
	oldPeers := ch.Meta().Peers
	extraPeers := make(map[string]charm.Relation)
	for relName, rel := range newPeers {
		if _, ok := oldPeers[relName]; !ok {
			extraPeers[relName] = rel
		}
	}
	return extraPeers
}

func (a *Application) checkRelationsOps(ch *Charm, relations []*Relation) ([]txn.Op, error) {
	asserts := make([]txn.Op, 0, len(relations))
	// All relations must still exist and their endpoints are implemented by the charm.
	for _, rel := range relations {
		if ep, err := rel.Endpoint(a.doc.Name); err != nil {
			return nil, err
		} else if !ep.ImplementedBy(ch) {
			return nil, errors.Errorf("would break relation %q", rel)
		}
		asserts = append(asserts, txn.Op{
			C:      relationsC,
			Id:     rel.doc.DocID,
			Assert: txn.DocExists,
		})
	}
	return asserts, nil
}

func (a *Application) checkStorageUpgrade(newMeta, oldMeta *charm.Meta, units []*Unit) (_ []txn.Op, err error) {
	// Make sure no storage instances are added or removed.
	var ops []txn.Op
	for name, oldStorageMeta := range oldMeta.Storage {
		if _, ok := newMeta.Storage[name]; ok {
			continue
		}
		if oldStorageMeta.CountMin > 0 {
			return nil, errors.Errorf("required storage %q removed", name)
		}
		// Optional storage has been removed. So long as there
		// are no instances of the store, it can safely be
		// removed.
		if oldStorageMeta.Shared {
			op, n, err := a.st.countEntityStorageInstances(a.Tag(), name)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if n > 0 {
				return nil, errors.Errorf("in-use storage %q removed", name)
			}
			ops = append(ops, op)
		} else {
			for _, u := range units {
				op, n, err := a.st.countEntityStorageInstances(u.Tag(), name)
				if err != nil {
					return nil, errors.Trace(err)
				}
				if n > 0 {
					return nil, errors.Errorf("in-use storage %q removed", name)
				}
				ops = append(ops, op)
			}
		}
	}
	less := func(a, b int) bool {
		return a != -1 && (b == -1 || a < b)
	}
	for name, newStorageMeta := range newMeta.Storage {
		oldStorageMeta, ok := oldMeta.Storage[name]
		if !ok {
			continue
		}
		if newStorageMeta.Type != oldStorageMeta.Type {
			return nil, errors.Errorf(
				"existing storage %q type changed from %q to %q",
				name, oldStorageMeta.Type, newStorageMeta.Type,
			)
		}
		if newStorageMeta.Shared != oldStorageMeta.Shared {
			return nil, errors.Errorf(
				"existing storage %q shared changed from %v to %v",
				name, oldStorageMeta.Shared, newStorageMeta.Shared,
			)
		}
		if newStorageMeta.ReadOnly != oldStorageMeta.ReadOnly {
			return nil, errors.Errorf(
				"existing storage %q read-only changed from %v to %v",
				name, oldStorageMeta.ReadOnly, newStorageMeta.ReadOnly,
			)
		}
		if newStorageMeta.Location != oldStorageMeta.Location {
			return nil, errors.Errorf(
				"existing storage %q location changed from %q to %q",
				name, oldStorageMeta.Location, newStorageMeta.Location,
			)
		}
		if less(newStorageMeta.CountMax, oldStorageMeta.CountMax) {
			var oldCountMax interface{} = oldStorageMeta.CountMax
			if oldStorageMeta.CountMax == -1 {
				oldCountMax = "<unbounded>"
			}
			return nil, errors.Errorf(
				"existing storage %q range contracted: max decreased from %v to %d",
				name, oldCountMax, newStorageMeta.CountMax,
			)
		}
		if oldStorageMeta.Location != "" && oldStorageMeta.CountMax == 1 && newStorageMeta.CountMax != 1 {
			// If a location is specified, the store may not go
			// from being a singleton to multiple, since then the
			// location has a different meaning.
			return nil, errors.Errorf(
				"existing storage %q with location changed from single to multiple",
				name,
			)
		}
	}
	return ops, nil
}

// changeCharmOps returns the operations necessary to set a application's
// charm URL to a new value.
func (a *Application) changeCharmOps(
	ch *Charm,
	channel string,
	updatedSettings charm.Settings,
	forceUnits bool,
	resourceIDs map[string]string,
	updatedStorageConstraints map[string]StorageConstraints,
) ([]txn.Op, error) {
	// Build the new application config from what can be used of the old one.
	var newSettings charm.Settings
	oldSettings, err := readSettings(a.st, settingsC, a.settingsKey())
	if err == nil {
		// Filter the old settings through to get the new settings.
		newSettings = ch.Config().FilterSettings(oldSettings.Map())
		for k, v := range updatedSettings {
			newSettings[k] = v
		}
	} else if errors.IsNotFound(err) {
		// No old settings, start with the updated settings.
		newSettings = updatedSettings
	} else {
		return nil, errors.Trace(err)
	}

	// Create or replace application settings.
	var settingsOp txn.Op
	newSettingsKey := applicationSettingsKey(a.doc.Name, ch.URL())
	if _, err := readSettings(a.st, settingsC, newSettingsKey); errors.IsNotFound(err) {
		// No settings for this key yet, create it.
		settingsOp = createSettingsOp(settingsC, newSettingsKey, newSettings)
	} else if err != nil {
		return nil, errors.Trace(err)
	} else {
		// Settings exist, just replace them with the new ones.
		settingsOp, _, err = replaceSettingsOp(a.st, settingsC, newSettingsKey, newSettings)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// Make sure no units are added or removed while the upgrade
	// transaction is being executed. This allows us to make
	// changes to units during the upgrade, e.g. add storage
	// to existing units, or remove optional storage so long as
	// it is unreferenced.
	units, err := a.AllUnits()
	if err != nil {
		return nil, errors.Trace(err)
	}
	unitOps := make([]txn.Op, len(units))
	for i, u := range units {
		unitOps[i] = txn.Op{
			C:      unitsC,
			Id:     u.doc.DocID,
			Assert: txn.DocExists,
		}
	}
	unitOps = append(unitOps, txn.Op{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Assert: bson.D{{"unitcount", len(units)}},
	})

	// Check storage to ensure no referenced storage is removed, or changed
	// in an incompatible way. We do this before computing the new storage
	// constraints, as incompatible charm changes will otherwise yield
	// confusing error messages that would suggest the user has supplied
	// invalid constraints.
	oldCharm, _, err := a.Charm()
	if err != nil {
		return nil, errors.Trace(err)
	}
	oldMeta := oldCharm.Meta()
	checkStorageOps, err := a.checkStorageUpgrade(ch.Meta(), oldMeta, units)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Create or replace storage constraints. We take the existing storage
	// constraints, remove any keys that are no longer referenced by the
	// charm, and update the constraints that the user has specified.
	var storageConstraintsOp txn.Op
	oldStorageConstraints, err := a.StorageConstraints()
	if err != nil {
		return nil, errors.Trace(err)
	}
	newStorageConstraints := oldStorageConstraints
	for name, cons := range updatedStorageConstraints {
		newStorageConstraints[name] = cons
	}
	for name := range newStorageConstraints {
		if _, ok := ch.Meta().Storage[name]; !ok {
			delete(newStorageConstraints, name)
		}
	}
	if err := addDefaultStorageConstraints(a.st, newStorageConstraints, ch.Meta()); err != nil {
		return nil, errors.Annotate(err, "adding default storage constraints")
	}
	if err := validateStorageConstraints(a.st, newStorageConstraints, ch.Meta()); err != nil {
		return nil, errors.Annotate(err, "validating storage constraints")
	}
	newStorageConstraintsKey := applicationStorageConstraintsKey(a.doc.Name, ch.URL())
	if _, err := readStorageConstraints(a.st, newStorageConstraintsKey); errors.IsNotFound(err) {
		storageConstraintsOp = createStorageConstraintsOp(
			newStorageConstraintsKey, newStorageConstraints,
		)
	} else if err != nil {
		return nil, errors.Trace(err)
	} else {
		storageConstraintsOp = replaceStorageConstraintsOp(
			newStorageConstraintsKey, newStorageConstraints,
		)
	}

	// Upgrade charm storage.
	upgradeStorageOps, err := a.upgradeStorageOps(ch.Meta(), oldMeta, units, newStorageConstraints)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Add or create a reference to the new charm, settings,
	// and storage constraints docs.
	incOps, err := appCharmIncRefOps(a.st, a.doc.Name, ch.URL(), true)
	if err != nil {
		return nil, errors.Trace(err)
	}
	var decOps []txn.Op
	// Drop the references to the old settings, storage constraints,
	// and charm docs (if the refs actually exist yet).
	if oldSettings != nil {
		decOps, err = appCharmDecRefOps(a.st, a.doc.Name, a.doc.CharmURL) // current charm
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// Build the transaction.
	var ops []txn.Op
	if oldSettings != nil {
		// Old settings shouldn't change (when they exist).
		ops = append(ops, oldSettings.assertUnchangedOp())
	}
	ops = append(ops, unitOps...)
	ops = append(ops, incOps...)
	ops = append(ops, []txn.Op{
		// Create or replace new settings.
		settingsOp,
		// Create storage constraints.
		storageConstraintsOp,
		// Update the charm URL and force flag (if relevant).
		{
			C:  applicationsC,
			Id: a.doc.DocID,
			Update: bson.D{{"$set", bson.D{
				{"charmurl", ch.URL()},
				{"cs-channel", channel},
				{"forcecharm", forceUnits},
			}}},
		},
	}...)
	ops = append(ops, checkStorageOps...)
	ops = append(ops, upgradeStorageOps...)

	ops = append(ops, incCharmModifiedVersionOps(a.doc.DocID)...)

	// Add any extra peer relations that need creation.
	newPeers := a.extraPeerRelations(ch.Meta())
	peerOps, err := a.st.addPeerRelationsOps(a.doc.Name, newPeers)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if len(resourceIDs) > 0 {
		// Collect pending resource resolution operations.
		resOps, err := a.resolveResourceOps(resourceIDs)
		if err != nil {
			return nil, errors.Trace(err)
		}
		ops = append(ops, resOps...)
	}

	// Get all relations - we need to check them later.
	relations, err := a.Relations()
	if err != nil {
		return nil, errors.Trace(err)
	}
	// Make sure the relation count does not change.
	sameRelCount := bson.D{{"relationcount", len(relations)}}

	ops = append(ops, peerOps...)
	// Update the relation count as well.
	ops = append(ops, txn.Op{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Assert: append(notDeadDoc, sameRelCount...),
		Update: bson.D{{"$inc", bson.D{{"relationcount", len(newPeers)}}}},
	})
	// Check relations to ensure no active relations are removed.
	relOps, err := a.checkRelationsOps(ch, relations)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops = append(ops, relOps...)

	// Update any existing endpoint bindings, using defaults for new endpoints.
	//
	// TODO(dimitern): Once upgrade-charm accepts --bind like deploy, pass the
	// given bindings below, instead of nil.
	endpointBindingsOp, err := updateEndpointBindingsOp(a.st, a.globalKey(), nil, ch.Meta())
	if err == nil {
		ops = append(ops, endpointBindingsOp)
	} else if !errors.IsNotFound(err) && err != jujutxn.ErrNoOperations {
		// If endpoint bindings do not exist this most likely means the application
		// itself no longer exists, which will be caught soon enough anyway.
		// ErrNoOperations on the other hand means there's nothing to update.
		return nil, errors.Trace(err)
	}

	// And finally, decrement the old charm and settings.
	return append(ops, decOps...), nil
}

func (a *Application) upgradeStorageOps(
	meta, oldMeta *charm.Meta,
	units []*Unit,
	allStorageCons map[string]StorageConstraints,
) (_ []txn.Op, err error) {
	// For each store, ensure that every unit has the minimum requirements.
	// If a unit has an existing store, but its minimum count has been
	// increased, we only add the shortfall; we do not necessarily add as
	// many instances as are specified in the storage constraints.
	var ops []txn.Op
	for name, cons := range allStorageCons {
		for _, u := range units {
			countMin := meta.Storage[name].CountMin
			if _, ok := oldMeta.Storage[name]; !ok {
				// The store did not exist previously, so we
				// create the full amount specified in the
				// cosntraints.
				countMin = int(cons.Count)
			}
			unitOps, err := a.st.addUnitStorageOps(
				meta, u, name, cons, countMin,
			)
			if err != nil {
				return nil, errors.Trace(err)
			}
			ops = append(ops, unitOps...)
		}
	}
	return ops, nil
}

// incCharmModifiedVersionOps returns the operations necessary to increment
// the CharmModifiedVersion field for the given application.
func incCharmModifiedVersionOps(applicationID string) []txn.Op {
	return []txn.Op{{
		C:      applicationsC,
		Id:     applicationID,
		Assert: txn.DocExists,
		Update: bson.D{{"$inc", bson.D{{"charmmodifiedversion", 1}}}},
	}}
}

func (a *Application) resolveResourceOps(resourceIDs map[string]string) ([]txn.Op, error) {
	// Collect pending resource resolution operations.
	resources, err := a.st.Resources()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return resources.NewResolvePendingResourcesOps(a.doc.Name, resourceIDs)
}

// SetCharmConfig contains the parameters for Application.SetCharm.
type SetCharmConfig struct {
	// Charm is the new charm to use for the application. New units
	// will be started with this charm, and existing units will be
	// upgraded to use it.
	Charm *Charm

	// Channel is the charm store channel from which charm was pulled.
	Channel csparams.Channel

	// ConfigSettings is the charm config settings to apply when upgrading
	// the charm.
	ConfigSettings charm.Settings

	// ForceUnits forces the upgrade on units in an error state.
	ForceUnits bool

	// ForceSeries forces the use of the charm even if it is not one of
	// the charm's supported series.
	ForceSeries bool

	// ResourceIDs is a map of resource names to resource IDs to activate during
	// the upgrade.
	ResourceIDs map[string]string

	// StorageConstraints contains the constraints to add or update when
	// upgrading the charm.
	//
	// Any existing storage instances for the named stores will be
	// unaffected; the storage constraints will only be used for
	// provisioning new storage instances.
	StorageConstraints map[string]StorageConstraints
}

// SetCharm changes the charm for the application.
func (a *Application) SetCharm(cfg SetCharmConfig) (err error) {
	defer errors.DeferredAnnotatef(
		&err, "cannot upgrade application %q to charm %q", a, cfg.Charm,
	)
	if cfg.Charm.Meta().Subordinate != a.doc.Subordinate {
		return errors.Errorf("cannot change an application's subordinacy")
	}
	// For old style charms written for only one series, we still retain
	// this check. Newer charms written for multi-series have a URL
	// with series = "".
	if cfg.Charm.URL().Series != "" {
		if cfg.Charm.URL().Series != a.doc.Series {
			return errors.Errorf("cannot change an application's series")
		}
	} else if !cfg.ForceSeries {
		supported := false
		for _, series := range cfg.Charm.Meta().Series {
			if series == a.doc.Series {
				supported = true
				break
			}
		}
		if !supported {
			supportedSeries := "no series"
			if len(cfg.Charm.Meta().Series) > 0 {
				supportedSeries = strings.Join(cfg.Charm.Meta().Series, ", ")
			}
			return errors.Errorf("only these series are supported: %v", supportedSeries)
		}
	} else {
		// Even with forceSeries=true, we do not allow a charm to be used which is for
		// a different OS. This assumes the charm declares it has supported series which
		// we can check for OS compatibility. Otherwise, we just accept the series supplied.
		currentOS, err := series.GetOSFromSeries(a.doc.Series)
		if err != nil {
			// We don't expect an error here but there's not much we can
			// do to recover.
			return err
		}
		supportedOS := false
		supportedSeries := cfg.Charm.Meta().Series
		for _, chSeries := range supportedSeries {
			charmSeriesOS, err := series.GetOSFromSeries(chSeries)
			if err != nil {
				return nil
			}
			if currentOS == charmSeriesOS {
				supportedOS = true
				break
			}
		}
		if !supportedOS && len(supportedSeries) > 0 {
			return errors.Errorf("OS %q not supported by charm", currentOS)
		}
	}

	updatedSettings, err := cfg.Charm.Config().ValidateSettings(cfg.ConfigSettings)
	if err != nil {
		return errors.Annotate(err, "validating config settings")
	}

	var newCharmModifiedVersion int
	channel := string(cfg.Channel)
	acopy := &Application{a.st, a.doc}
	buildTxn := func(attempt int) ([]txn.Op, error) {
		a := acopy
		if attempt > 0 {
			if err := a.Refresh(); err != nil {
				return nil, errors.Trace(err)
			}
		}

		// NOTE: We're explicitly allowing SetCharm to succeed
		// when the application is Dying, because application/charm
		// upgrades should still be allowed to apply to dying
		// applications and units, so that bugs in departed/broken
		// hooks can be addressed at runtime.
		if a.Life() == Dead {
			return nil, ErrDead
		}

		// Record the current value of charmModifiedVersion, so we can
		// set the value on the method receiver's in-memory document
		// structure. We increment the version only when we change the
		// charm URL.
		newCharmModifiedVersion = a.doc.CharmModifiedVersion

		ops := []txn.Op{{
			C:  applicationsC,
			Id: a.doc.DocID,
			Assert: append(notDeadDoc, bson.DocElem{
				"charmmodifiedversion", a.doc.CharmModifiedVersion,
			}),
		}}

		if a.doc.CharmURL.String() == cfg.Charm.URL().String() {
			// Charm URL already set; just update the force flag and channel.
			ops = append(ops, txn.Op{
				C:  applicationsC,
				Id: a.doc.DocID,
				Update: bson.D{{"$set", bson.D{
					{"cs-channel", channel},
					{"forcecharm", cfg.ForceUnits},
				}}},
			})
		} else {
			chng, err := a.changeCharmOps(
				cfg.Charm,
				channel,
				updatedSettings,
				cfg.ForceUnits,
				cfg.ResourceIDs,
				cfg.StorageConstraints,
			)
			if err != nil {
				return nil, errors.Trace(err)
			}
			ops = append(ops, chng...)
			newCharmModifiedVersion++
		}

		return ops, nil
	}
	if err := a.st.run(buildTxn); err != nil {
		return err
	}
	a.doc.CharmURL = cfg.Charm.URL()
	a.doc.Channel = channel
	a.doc.ForceCharm = cfg.ForceUnits
	a.doc.CharmModifiedVersion = newCharmModifiedVersion
	return nil
}

// String returns the application name.
func (a *Application) String() string {
	return a.doc.Name
}

// Refresh refreshes the contents of the Application from the underlying
// state. It returns an error that satisfies errors.IsNotFound if the
// application has been removed.
func (a *Application) Refresh() error {
	applications, closer := a.st.getCollection(applicationsC)
	defer closer()

	err := applications.FindId(a.doc.DocID).One(&a.doc)
	if err == mgo.ErrNotFound {
		return errors.NotFoundf("application %q", a)
	}
	if err != nil {
		return errors.Errorf("cannot refresh application %q: %v", a, err)
	}
	return nil
}

// newUnitName returns the next unit name.
func (a *Application) newUnitName() (string, error) {
	unitSeq, err := a.st.sequence(a.Tag().String())
	if err != nil {
		return "", errors.Trace(err)
	}
	name := a.doc.Name + "/" + strconv.Itoa(unitSeq)
	return name, nil
}

// addUnitOps returns a unique name for a new unit, and a list of txn operations
// necessary to create that unit. The principalName param must be non-empty if
// and only if s is a subordinate application. Only one subordinate of a given
// application will be assigned to a given principal. The asserts param can be used
// to include additional assertions for the application document.  This method
// assumes that the application already exists in the db.
func (a *Application) addUnitOps(principalName string, asserts bson.D) (string, []txn.Op, error) {
	var cons constraints.Value
	if !a.doc.Subordinate {
		scons, err := a.Constraints()
		if errors.IsNotFound(err) {
			return "", nil, errors.NotFoundf("application %q", a.Name())
		}
		if err != nil {
			return "", nil, err
		}
		cons, err = a.st.resolveConstraints(scons)
		if err != nil {
			return "", nil, err
		}
	}
	storageCons, err := a.StorageConstraints()
	if err != nil {
		return "", nil, err
	}
	args := applicationAddUnitOpsArgs{
		cons:          cons,
		principalName: principalName,
		storageCons:   storageCons,
	}
	names, ops, err := a.addUnitOpsWithCons(args)
	if err != nil {
		return names, ops, err
	}
	// we verify the application is alive
	asserts = append(isAliveDoc, asserts...)
	ops = append(ops, a.incUnitCountOp(asserts))
	return names, ops, err
}

type applicationAddUnitOpsArgs struct {
	principalName string
	cons          constraints.Value
	storageCons   map[string]StorageConstraints
}

// addApplicationUnitOps is just like addUnitOps but explicitly takes a
// constraints value (this is used at application creation time).
func (a *Application) addApplicationUnitOps(args applicationAddUnitOpsArgs) (string, []txn.Op, error) {
	names, ops, err := a.addUnitOpsWithCons(args)
	if err == nil {
		ops = append(ops, a.incUnitCountOp(nil))
	}
	return names, ops, err
}

// addUnitOpsWithCons is a helper method for returning addUnitOps.
func (a *Application) addUnitOpsWithCons(args applicationAddUnitOpsArgs) (string, []txn.Op, error) {
	if a.doc.Subordinate && args.principalName == "" {
		return "", nil, errors.New("application is a subordinate")
	} else if !a.doc.Subordinate && args.principalName != "" {
		return "", nil, errors.New("application is not a subordinate")
	}
	name, err := a.newUnitName()
	if err != nil {
		return "", nil, err
	}
	unitTag := names.NewUnitTag(name)

	charm, _, err := a.Charm()
	if err != nil {
		return "", nil, err
	}

	// Add storage instances/attachments for the unit. If the
	// application is subordinate, we'll add the machine storage
	// if the principal is assigned to a machine. Otherwise, we
	// will add the subordinate's storage along with the principal's
	// when the principal is assigned to a machine.
	var machineAssignable machineAssignable
	if a.doc.Subordinate {
		pu, err := a.st.Unit(args.principalName)
		if err != nil {
			return "", nil, errors.Trace(err)
		}
		machineAssignable = pu
	}
	storageOps, numStorageAttachments, err := createStorageOps(
		a.st,
		unitTag,
		charm.Meta(),
		args.storageCons,
		a.doc.Series,
		machineAssignable,
	)
	if err != nil {
		return "", nil, errors.Trace(err)
	}

	docID := a.st.docID(name)
	globalKey := unitGlobalKey(name)
	agentGlobalKey := unitAgentGlobalKey(name)
	udoc := &unitDoc{
		DocID:                  docID,
		Name:                   name,
		Application:            a.doc.Name,
		Series:                 a.doc.Series,
		Life:                   Alive,
		Principal:              args.principalName,
		StorageAttachmentCount: numStorageAttachments,
	}
	now := a.st.clock.Now()
	agentStatusDoc := statusDoc{
		Status:  status.Allocating,
		Updated: now.UnixNano(),
	}
	unitStatusDoc := statusDoc{
		Status:     status.Waiting,
		StatusInfo: status.MessageWaitForMachine,
		Updated:    now.UnixNano(),
	}
	workloadVersionDoc := statusDoc{
		Status:  status.Unknown,
		Updated: now.UnixNano(),
	}

	ops, err := addUnitOps(a.st, addUnitOpsArgs{
		unitDoc:            udoc,
		agentStatusDoc:     agentStatusDoc,
		workloadStatusDoc:  unitStatusDoc,
		workloadVersionDoc: workloadVersionDoc,
		meterStatusDoc:     &meterStatusDoc{Code: MeterNotSet.String()},
	})
	if err != nil {
		return "", nil, errors.Trace(err)
	}

	ops = append(ops, storageOps...)

	if a.doc.Subordinate {
		ops = append(ops, txn.Op{
			C:  unitsC,
			Id: a.st.docID(args.principalName),
			Assert: append(isAliveDoc, bson.DocElem{
				"subordinates", bson.D{{"$not", bson.RegEx{Pattern: "^" + a.doc.Name + "/"}}},
			}),
			Update: bson.D{{"$addToSet", bson.D{{"subordinates", name}}}},
		})
	} else {
		ops = append(ops, createConstraintsOp(a.st, agentGlobalKey, args.cons))
	}

	// At the last moment we still have the statusDocs in scope, set the initial
	// history entries. This is risky, and may lead to extra entries, but that's
	// an intrinsic problem with mixing txn and non-txn ops -- we can't sync
	// them cleanly.
	probablyUpdateStatusHistory(a.st, globalKey, unitStatusDoc)
	probablyUpdateStatusHistory(a.st, agentGlobalKey, agentStatusDoc)
	probablyUpdateStatusHistory(a.st, globalWorkloadVersionKey(name), workloadVersionDoc)
	return name, ops, nil
}

// incUnitCountOp returns the operation to increment the application's unit count.
func (a *Application) incUnitCountOp(asserts bson.D) txn.Op {
	op := txn.Op{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Update: bson.D{{"$inc", bson.D{{"unitcount", 1}}}},
	}
	if len(asserts) > 0 {
		op.Assert = asserts
	}
	return op
}

// AddUnit adds a new principal unit to the application.
func (a *Application) AddUnit() (unit *Unit, err error) {
	defer errors.DeferredAnnotatef(&err, "cannot add unit to application %q", a)
	name, ops, err := a.addUnitOps("", nil)
	if err != nil {
		return nil, err
	}

	if err := a.st.runTransaction(ops); err == txn.ErrAborted {
		if alive, err := isAlive(a.st, applicationsC, a.doc.DocID); err != nil {
			return nil, err
		} else if !alive {
			return nil, errors.New("application is not alive")
		}
		return nil, errors.New("inconsistent state")
	} else if err != nil {
		return nil, err
	}
	return a.st.Unit(name)
}

// removeUnitOps returns the operations necessary to remove the supplied unit,
// assuming the supplied asserts apply to the unit document.
func (a *Application) removeUnitOps(u *Unit, asserts bson.D) ([]txn.Op, error) {
	ops, err := u.destroyHostOps(a)
	if err != nil {
		return nil, err
	}
	portsOps, err := removePortsForUnitOps(a.st, u)
	if err != nil {
		return nil, err
	}
	storageInstanceOps, err := removeStorageInstancesOps(a.st, u.Tag())
	if err != nil {
		return nil, err
	}
	resOps, err := removeUnitResourcesOps(a.st, u.doc.Name)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops = append(ops, resOps...)

	observedFieldsMatch := bson.D{
		{"charmurl", u.doc.CharmURL},
		{"machineid", u.doc.MachineId},
	}
	ops = append(ops,
		txn.Op{
			C:      unitsC,
			Id:     u.doc.DocID,
			Assert: append(observedFieldsMatch, asserts...),
			Remove: true,
		},
		removeMeterStatusOp(a.st, u.globalMeterStatusKey()),
		removeStatusOp(a.st, u.globalAgentKey()),
		removeStatusOp(a.st, u.globalKey()),
		removeConstraintsOp(a.st, u.globalAgentKey()),
		annotationRemoveOp(a.st, u.globalKey()),
		newCleanupOp(cleanupRemovedUnit, u.doc.Name),
	)
	ops = append(ops, portsOps...)
	ops = append(ops, storageInstanceOps...)
	if u.doc.CharmURL != nil {
		decOps, err := appCharmDecRefOps(a.st, a.doc.Name, u.doc.CharmURL)
		if errors.IsNotFound(err) {
			return nil, errRefresh
		} else if err != nil {
			return nil, err
		}
		ops = append(ops, decOps...)
	}
	if a.doc.Life == Dying && a.doc.RelationCount == 0 && a.doc.UnitCount == 1 {
		hasLastRef := bson.D{{"life", Dying}, {"relationcount", 0}, {"unitcount", 1}}
		removeOps, err := a.removeOps(hasLastRef)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return append(ops, removeOps...), nil
	}
	svcOp := txn.Op{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Update: bson.D{{"$inc", bson.D{{"unitcount", -1}}}},
	}
	if a.doc.Life == Alive {
		svcOp.Assert = bson.D{{"life", Alive}, {"unitcount", bson.D{{"$gt", 0}}}}
	} else {
		svcOp.Assert = bson.D{
			{"life", Dying},
			{"$or", []bson.D{
				{{"unitcount", bson.D{{"$gt", 1}}}},
				{{"relationcount", bson.D{{"$gt", 0}}}},
			}},
		}
	}
	ops = append(ops, svcOp)

	return ops, nil
}

func removeUnitResourcesOps(st *State, unitID string) ([]txn.Op, error) {
	persist, err := st.ResourcesPersistence()
	if errors.IsNotSupported(err) {
		// Nothing to see here, move along.
		return nil, nil
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	ops, err := persist.NewRemoveUnitResourcesOps(unitID)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return ops, nil
}

// AllUnits returns all units of the application.
func (a *Application) AllUnits() (units []*Unit, err error) {
	return allUnits(a.st, a.doc.Name)
}

func allUnits(st *State, application string) (units []*Unit, err error) {
	unitsCollection, closer := st.getCollection(unitsC)
	defer closer()

	docs := []unitDoc{}
	err = unitsCollection.Find(bson.D{{"application", application}}).All(&docs)
	if err != nil {
		return nil, errors.Errorf("cannot get all units from application %q: %v", application, err)
	}
	for i := range docs {
		units = append(units, newUnit(st, &docs[i]))
	}
	return units, nil
}

// Relations returns a Relation for every relation the application is in.
func (a *Application) Relations() (relations []*Relation, err error) {
	return applicationRelations(a.st, a.doc.Name)
}

func applicationRelations(st *State, name string) (relations []*Relation, err error) {
	defer errors.DeferredAnnotatef(&err, "can't get relations for application %q", name)
	relationsCollection, closer := st.getCollection(relationsC)
	defer closer()

	docs := []relationDoc{}
	err = relationsCollection.Find(bson.D{{"endpoints.applicationname", name}}).All(&docs)
	if err != nil {
		return nil, err
	}
	for _, v := range docs {
		relations = append(relations, newRelation(st, &v))
	}
	return relations, nil
}

// ConfigSettings returns the raw user configuration for the application's charm.
// Unset values are omitted.
func (a *Application) ConfigSettings() (charm.Settings, error) {
	settings, err := readSettings(a.st, settingsC, a.settingsKey())
	if err != nil {
		return nil, err
	}
	return settings.Map(), nil
}

// UpdateConfigSettings changes a application's charm config settings. Values set
// to nil will be deleted; unknown and invalid values will return an error.
func (a *Application) UpdateConfigSettings(changes charm.Settings) error {
	charm, _, err := a.Charm()
	if err != nil {
		return err
	}
	changes, err = charm.Config().ValidateSettings(changes)
	if err != nil {
		return err
	}
	// TODO(fwereade) state.Settings is itself really problematic in just
	// about every use case. This needs to be resolved some time; but at
	// least the settings docs are keyed by charm url as well as application
	// name, so the actual impact of a race is non-threatening.
	node, err := readSettings(a.st, settingsC, a.settingsKey())
	if err != nil {
		return err
	}
	for name, value := range changes {
		if value == nil {
			node.Delete(name)
		} else {
			node.Set(name, value)
		}
	}
	_, err = node.Write()
	return err
}

// LeaderSettings returns a application's leader settings. If nothing has been set
// yet, it will return an empty map; this is not an error.
func (a *Application) LeaderSettings() (map[string]string, error) {
	// There's no compelling reason to have these methods on Application -- and
	// thus require an extra db read to access them -- but it stops the State
	// type getting even more cluttered.

	doc, err := readSettingsDoc(a.st, settingsC, leadershipSettingsKey(a.doc.Name))
	if errors.IsNotFound(err) {
		return nil, errors.NotFoundf("application")
	} else if err != nil {
		return nil, errors.Trace(err)
	}
	result := make(map[string]string)
	for escapedKey, interfaceValue := range doc.Settings {
		key := unescapeReplacer.Replace(escapedKey)
		if value, _ := interfaceValue.(string); value != "" {
			// Empty strings are technically bad data -- when set, they clear.
			result[key] = value
		} else {
			// Some bad data isn't reason enough to obscure the good data.
			logger.Warningf("unexpected leader settings value for %s: %#v", key, interfaceValue)
		}
	}
	return result, nil
}

// UpdateLeaderSettings updates the application's leader settings with the supplied
// values, but will fail (with a suitable error) if the supplied Token loses
// validity. Empty values in the supplied map will be cleared in the database.
func (a *Application) UpdateLeaderSettings(token leadership.Token, updates map[string]string) error {
	// There's no compelling reason to have these methods on Application -- and
	// thus require an extra db read to access them -- but it stops the State
	// type getting even more cluttered.

	// We can calculate the actual update ahead of time; it's not dependent
	// upon the current state of the document. (*Writing* it should depend
	// on document state, but that's handled below.)
	key := leadershipSettingsKey(a.doc.Name)
	sets := bson.M{}
	unsets := bson.M{}
	for unescapedKey, value := range updates {
		key := escapeReplacer.Replace(unescapedKey)
		if value == "" {
			unsets[key] = 1
		} else {
			sets[key] = value
		}
	}
	update := setUnsetUpdateSettings(sets, unsets)

	isNullChange := func(rawMap map[string]interface{}) bool {
		for key := range unsets {
			if _, found := rawMap[key]; found {
				return false
			}
		}
		for key, value := range sets {
			if current := rawMap[key]; current != value {
				return false
			}
		}
		return true
	}

	buildTxn := func(_ int) ([]txn.Op, error) {
		// Read the current document state so we can abort if there's
		// no actual change; and the version number so we can assert
		// on it and prevent these settings from landing late.
		doc, err := readSettingsDoc(a.st, settingsC, key)
		if errors.IsNotFound(err) {
			return nil, errors.NotFoundf("application")
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		if isNullChange(doc.Settings) {
			return nil, jujutxn.ErrNoOperations
		}
		return []txn.Op{{
			C:      settingsC,
			Id:     key,
			Assert: bson.D{{"version", doc.Version}},
			Update: update,
		}}, nil
	}
	return a.st.run(buildTxnWithLeadership(buildTxn, token))
}

var ErrSubordinateConstraints = stderrors.New("constraints do not apply to subordinate applications")

// Constraints returns the current application constraints.
func (a *Application) Constraints() (constraints.Value, error) {
	if a.doc.Subordinate {
		return constraints.Value{}, ErrSubordinateConstraints
	}
	return readConstraints(a.st, a.globalKey())
}

// SetConstraints replaces the current application constraints.
func (a *Application) SetConstraints(cons constraints.Value) (err error) {
	unsupported, err := a.st.validateConstraints(cons)
	if len(unsupported) > 0 {
		logger.Warningf(
			"setting constraints on application %q: unsupported constraints: %v", a.Name(), strings.Join(unsupported, ","))
	} else if err != nil {
		return err
	}
	if a.doc.Subordinate {
		return ErrSubordinateConstraints
	}
	defer errors.DeferredAnnotatef(&err, "cannot set constraints")
	if a.doc.Life != Alive {
		return errNotAlive
	}
	ops := []txn.Op{{
		C:      applicationsC,
		Id:     a.doc.DocID,
		Assert: isAliveDoc,
	}}
	ops = append(ops, setConstraintsOp(a.st, a.globalKey(), cons))
	return onAbort(a.st.runTransaction(ops), errNotAlive)
}

// EndpointBindings returns the mapping for each endpoint name and the space
// name it is bound to (or empty if unspecified). When no bindings are stored
// for the application, defaults are returned.
func (a *Application) EndpointBindings() (map[string]string, error) {
	// We don't need the TxnRevno below.
	bindings, _, err := readEndpointBindings(a.st, a.globalKey())
	if err != nil && !errors.IsNotFound(err) {
		return nil, errors.Trace(err)
	}
	if bindings == nil {
		bindings, err = a.defaultEndpointBindings()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return bindings, nil
}

// defaultEndpointBindings returns a map with each endpoint from the current
// charm metadata bound to an empty space. If no charm URL is set yet, it
// returns an empty map.
func (a *Application) defaultEndpointBindings() (map[string]string, error) {
	if a.doc.CharmURL == nil {
		return map[string]string{}, nil
	}

	charm, _, err := a.Charm()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return DefaultEndpointBindingsForCharm(charm.Meta()), nil
}

// MetricCredentials returns any metric credentials associated with this application.
func (a *Application) MetricCredentials() []byte {
	return a.doc.MetricCredentials
}

// SetMetricCredentials updates the metric credentials associated with this application.
func (a *Application) SetMetricCredentials(b []byte) error {
	buildTxn := func(attempt int) ([]txn.Op, error) {
		if attempt > 0 {
			alive, err := isAlive(a.st, applicationsC, a.doc.DocID)
			if err != nil {
				return nil, errors.Trace(err)
			} else if !alive {
				return nil, errNotAlive
			}
		}
		ops := []txn.Op{
			{
				C:      applicationsC,
				Id:     a.doc.DocID,
				Assert: isAliveDoc,
				Update: bson.M{"$set": bson.M{"metric-credentials": b}},
			},
		}
		return ops, nil
	}
	if err := a.st.run(buildTxn); err != nil {
		if err == errNotAlive {
			return errors.New("cannot update metric credentials: application " + err.Error())
		}
		return errors.Annotatef(err, "cannot update metric credentials")
	}
	a.doc.MetricCredentials = b
	return nil
}

// StorageConstraints returns the storage constraints for the application.
func (a *Application) StorageConstraints() (map[string]StorageConstraints, error) {
	cons, err := readStorageConstraints(a.st, a.storageConstraintsKey())
	if errors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, errors.Trace(err)
	}
	return cons, nil
}

// Status returns the status of the application.
// Only unit leaders are allowed to set the status of the application.
// If no status is recorded, then there are no unit leaders and the
// status is derived from the unit status values.
func (a *Application) Status() (status.StatusInfo, error) {
	statuses, closer := a.st.getCollection(statusesC)
	defer closer()
	query := statuses.Find(bson.D{{"_id", a.globalKey()}, {"neverset", true}})
	if count, err := query.Count(); err != nil {
		return status.StatusInfo{}, errors.Trace(err)
	} else if count != 0 {
		// This indicates that SetStatus has never been called on this application.
		// This in turn implies the application status document is likely to be
		// inaccurate, so we return aggregated unit statuses instead.
		//
		// TODO(fwereade): this is completely wrong and will produce bad results
		// in not-very-challenging scenarios. The leader unit remains responsible
		// for setting the application status in a timely way, *whether or not the
		// charm's hooks exists or sets an application status*. This logic should be
		// removed as soon as possible, and the responsibilities implemented in
		// the right places rather than being applied at seeming random.
		units, err := a.AllUnits()
		if err != nil {
			return status.StatusInfo{}, err
		}
		logger.Tracef("application %q has %d units", a.Name(), len(units))
		if len(units) > 0 {
			return a.deriveStatus(units)
		}
	}
	return getStatus(a.st, a.globalKey(), "application")
}

// SetStatus sets the status for the application.
func (a *Application) SetStatus(statusInfo status.StatusInfo) error {
	if !status.ValidWorkloadStatus(statusInfo.Status) {
		return errors.Errorf("cannot set invalid status %q", statusInfo.Status)
	}
	return setStatus(a.st, setStatusParams{
		badge:     "application",
		globalKey: a.globalKey(),
		status:    statusInfo.Status,
		message:   statusInfo.Message,
		rawData:   statusInfo.Data,
		updated:   statusInfo.Since,
	})
}

// StatusHistory returns a slice of at most filter.Size StatusInfo items
// or items as old as filter.Date or items newer than now - filter.Delta time
// representing past statuses for this application.
func (a *Application) StatusHistory(filter status.StatusHistoryFilter) ([]status.StatusInfo, error) {
	args := &statusHistoryArgs{
		st:        a.st,
		globalKey: a.globalKey(),
		filter:    filter,
	}
	return statusHistory(args)
}

// ApplicationAndUnitsStatus returns the status for this application and all its units.
func (a *Application) ApplicationAndUnitsStatus() (status.StatusInfo, map[string]status.StatusInfo, error) {
	applicationStatus, err := a.Status()
	if err != nil {
		return status.StatusInfo{}, nil, errors.Trace(err)
	}
	units, err := a.AllUnits()
	if err != nil {
		return status.StatusInfo{}, nil, err
	}
	results := make(map[string]status.StatusInfo, len(units))
	for _, unit := range units {
		unitStatus, err := unit.Status()
		if err != nil {
			return status.StatusInfo{}, nil, err
		}
		results[unit.Name()] = unitStatus
	}
	return applicationStatus, results, nil

}

func (a *Application) deriveStatus(units []*Unit) (status.StatusInfo, error) {
	var result status.StatusInfo
	for _, unit := range units {
		currentSeverity := statusServerities[result.Status]
		unitStatus, err := unit.Status()
		if err != nil {
			return status.StatusInfo{}, errors.Annotatef(err, "deriving application status from %q", unit.Name())
		}
		unitSeverity := statusServerities[unitStatus.Status]
		if unitSeverity > currentSeverity {
			result.Status = unitStatus.Status
			result.Message = unitStatus.Message
			result.Data = unitStatus.Data
			result.Since = unitStatus.Since
		}
	}
	return result, nil
}

// statusSeverities holds status values with a severity measure.
// Status values with higher severity are used in preference to others.
var statusServerities = map[status.Status]int{
	status.Error:       100,
	status.Blocked:     90,
	status.Waiting:     80,
	status.Maintenance: 70,
	status.Terminated:  60,
	status.Active:      50,
	status.Unknown:     40,
}

type addApplicationOpsArgs struct {
	applicationDoc *applicationDoc
	statusDoc      statusDoc
	constraints    constraints.Value
	storage        map[string]StorageConstraints
	settings       map[string]interface{}
	// These are nil when adding a new application, and most likely
	// non-nil when migrating.
	leadershipSettings map[string]interface{}
}

// addApplicationOps returns the operations required to add an application to the
// applications collection, along with all the associated expected other application
// entries. This method is used by both the *State.AddApplication method and the
// migration import code.
func addApplicationOps(st *State, app *Application, args addApplicationOpsArgs) ([]txn.Op, error) {
	charmRefOps, err := appCharmIncRefOps(st, args.applicationDoc.Name, args.applicationDoc.CharmURL, true)
	if err != nil {
		return nil, errors.Trace(err)
	}

	globalKey := app.globalKey()
	settingsKey := app.settingsKey()
	storageConstraintsKey := app.storageConstraintsKey()
	leadershipKey := leadershipSettingsKey(app.Name())

	ops := []txn.Op{
		createConstraintsOp(st, globalKey, args.constraints),
		createStorageConstraintsOp(storageConstraintsKey, args.storage),
		createSettingsOp(settingsC, settingsKey, args.settings),
		createSettingsOp(settingsC, leadershipKey, args.leadershipSettings),
		createStatusOp(st, globalKey, args.statusDoc),
		addModelApplicationRefOp(st, app.Name()),
	}
	ops = append(ops, charmRefOps...)
	ops = append(ops, txn.Op{
		C:      applicationsC,
		Id:     app.Name(),
		Assert: txn.DocMissing,
		Insert: args.applicationDoc,
	})
	if featureflag.Enabled(feature.CrossModelRelations) {
		ops = append(ops, txn.Op{
			C:      remoteApplicationsC,
			Id:     app.Name(),
			Assert: txn.DocMissing,
		})
	}
	return ops, nil
}
