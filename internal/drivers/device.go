package drivers

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// ModelSoftphone is Device.Model for every Softphone: the softphone driver's
// only model.
const ModelSoftphone = "softphone"

// DeviceIDMaxLength is the longest device ID DeviceID returns. It is the
// tightest limit any device puts on the ID: the 79x5's authName (its SIP
// auth username) holds at most 30 characters (usecallmanager.nz,
// SEPMAC.cnf.xml), so a longer ID could never register.
const DeviceIDMaxLength = 30

// DeviceID returns a device's ID in the compiled bundle: the kind in lower
// case, a dash, and the resource name, e.g. "phone-desk" or
// "softphone-chris-laptop". The kind keeps a Phone and a Softphone with the
// same name apart.
//
// The ID names the device's PJSIP endpoint and is its SIP auth username
// (the 79x5's authName, Linphone's userid); its number is the SIP user and
// AOR (see Device.Number). It depends only on the resource, so it survives
// a change of number, MAC or label, and agent observations stay attached.
//
// An ID longer than DeviceIDMaxLength is shortened: the name is cut, and the
// first 8 hex digits of the name's SHA-256 are appended
// ("phone-<cut name>-1a2b3c4d"), so a Phone name of up to 24 characters and
// a Softphone name of up to 20 are kept whole. Only names deliberately
// crafted to collide with such an ID can produce two devices with one ID;
// Compile refuses that.
func DeviceID(kind DeviceKind, name string) string {
	prefix := strings.ToLower(string(kind)) + "-"
	if len(prefix)+len(name) <= DeviceIDMaxLength {
		return prefix + name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	keep := DeviceIDMaxLength - len(prefix) - len(suffix)
	return prefix + strings.TrimRight(name[:keep], ".-") + suffix
}

// RequestMaxLength is the longest request annotation value that counts as a
// request (v1alpha1: "at most 256 characters; a longer one is ignored").
const RequestMaxLength = 256

// PhoneDevice builds the Device for a Phone from its spec, its request
// annotation and its status: everything that comes from the resource alone.
// The caller sets what needs the whole install: ConfigVersion (from the
// compiled bundle) and the claim fields NumberConflict, MACConflict,
// NumberOutOfPlan and ClaimMessage (from compile.ResolveClaims). Memory and
// Display are deep copies, so a driver can change them freely.
func PhoneDevice(p *v1alpha1.Phone) Device {
	label := p.Spec.Label
	if label == "" {
		label = p.Name
	}
	st := &p.Status
	m := Memory{
		LastAction:     st.LastAction.DeepCopy(),
		HandledRequest: st.HandledRequest,
	}
	if o := st.Observed; o != nil {
		m.FirstSeen = o.FirstSeen.DeepCopy()
		m.Firmware = o.Firmware
		m.Address = o.Address
		m.LastConfigFetch = o.LastConfigFetch.DeepCopy()
		m.LastRegistered = o.LastRegistered.DeepCopy()
		m.UpgradeStartedAt = o.UpgradeStartedAt.DeepCopy()
	}
	return Device{
		Kind:      KindPhone,
		Name:      p.Name,
		ID:        DeviceID(KindPhone, p.Name),
		Model:     string(p.Spec.Model),
		MAC:       p.Spec.MAC.Normalized(),
		Number:    string(p.Spec.Number),
		Label:     label,
		Firmware:  p.Spec.Firmware,
		Display:   p.Spec.Display.DeepCopy(),
		Request:   request(p.Annotations, v1alpha1.AnnotationResyncRequested),
		LastState: State(st.State),
		Memory:    m,
	}
}

// SoftphoneDevice builds the Device for a Softphone, as PhoneDevice does for
// a Phone. Model is ModelSoftphone; MAC and Firmware are empty.
func SoftphoneDevice(s *v1alpha1.Softphone) Device {
	label := s.Spec.Label
	if label == "" {
		label = s.Name
	}
	st := &s.Status
	m := Memory{
		LastAction:     st.LastAction.DeepCopy(),
		HandledRequest: st.HandledRequest,
	}
	if o := st.Observed; o != nil {
		m.FirstSeen = o.FirstSeen.DeepCopy()
		m.Address = o.Address
		m.LastConfigFetch = o.LastConfigFetch.DeepCopy()
		m.LastRegistered = o.LastRegistered.DeepCopy()
	}
	return Device{
		Kind:      KindSoftphone,
		Name:      s.Name,
		ID:        DeviceID(KindSoftphone, s.Name),
		Model:     ModelSoftphone,
		Number:    string(s.Spec.Number),
		Label:     label,
		Request:   request(s.Annotations, v1alpha1.AnnotationReissueRequested),
		LastState: State(st.State),
		Memory:    m,
	}
}

// request returns the annotation's value when it counts as a request.
func request(annotations map[string]string, key string) string {
	v := annotations[key]
	if len(v) > RequestMaxLength {
		return ""
	}
	return v
}

// ToPhoneStatus writes the memory into a Phone's status: observed (nil when
// there is nothing to remember), lastAction and handledRequest. The rest of
// the status (state, conditions, humanAction) comes from the Decision.
func (m Memory) ToPhoneStatus(st *v1alpha1.PhoneStatus) {
	o := v1alpha1.PhoneObserved{
		FirstSeen:        m.FirstSeen.DeepCopy(),
		Firmware:         m.Firmware,
		Address:          m.Address,
		LastConfigFetch:  m.LastConfigFetch.DeepCopy(),
		LastRegistered:   m.LastRegistered.DeepCopy(),
		UpgradeStartedAt: m.UpgradeStartedAt.DeepCopy(),
	}
	st.Observed = nil
	if o.FirstSeen != nil || o.Firmware != "" || o.Address != "" || o.LastConfigFetch != nil ||
		o.LastRegistered != nil || o.UpgradeStartedAt != nil {
		st.Observed = &o
	}
	st.LastAction = m.LastAction.DeepCopy()
	st.HandledRequest = m.HandledRequest
}

// ToSoftphoneStatus writes the memory into a Softphone's status, as
// ToPhoneStatus does. A softphone has no firmware, so Firmware and
// UpgradeStartedAt are not kept.
func (m Memory) ToSoftphoneStatus(st *v1alpha1.SoftphoneStatus) {
	o := v1alpha1.SoftphoneObserved{
		FirstSeen:       m.FirstSeen.DeepCopy(),
		Address:         m.Address,
		LastConfigFetch: m.LastConfigFetch.DeepCopy(),
		LastRegistered:  m.LastRegistered.DeepCopy(),
	}
	st.Observed = nil
	if o.FirstSeen != nil || o.Address != "" || o.LastConfigFetch != nil || o.LastRegistered != nil {
		st.Observed = &o
	}
	st.LastAction = m.LastAction.DeepCopy()
	st.HandledRequest = m.HandledRequest
}
