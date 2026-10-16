package drivers

import "github.com/ChristopherJMiller/patchbay/api/v1alpha1"

// DisplayWithDefaults is the display a Phone gets: its own spec.display
// (Device.Display) with every field it leaves unset taken from the house
// default (RenderContext.DisplayDefaults), field by field. OnWindow is one
// field: the Phone's replaces the default's whole. Nil when both are nil,
// so a Phone with neither renders no display settings. The result is a
// fresh copy that shares nothing with either argument.
//
// A driver applies the default only to models whose profile supports
// FeatureDisplayPowerSave; a Phone whose own display asks for it on a
// model without it is Invalid (Unsupported).
func DisplayWithDefaults(own, house *v1alpha1.PhoneDisplay) *v1alpha1.PhoneDisplay {
	if own == nil && house == nil {
		return nil
	}
	out := own.DeepCopy()
	if out == nil {
		out = &v1alpha1.PhoneDisplay{}
	}
	if house == nil {
		return out
	}
	if out.IdleTimeout == nil {
		out.IdleTimeout = house.IdleTimeout.DeepCopy()
	}
	if out.WakeOnCall == nil && house.WakeOnCall != nil {
		wake := *house.WakeOnCall
		out.WakeOnCall = &wake
	}
	if out.OnWindow == nil {
		out.OnWindow = house.OnWindow.DeepCopy()
	}
	return out
}
