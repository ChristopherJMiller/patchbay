// Package softphone is the driver for software SIP clients provisioned by
// Linphone remote provisioning (design §4, §8.4): one model, "softphone",
// whose devices are v1alpha1.Softphone resources.
//
// A softphone is provisioned by scanning a QR code of a one-time URL. The
// agent serves the softphone's document (Render's provisioning/<ID>.xml) at
// that URL once; the controller then rotates the token (Decide's
// rotateToken action), so each QR code works once. Linphone keeps the
// settings it fetched and registers to the PBX over UDP.
//
// # The provisioning document
//
// Linphone's remote provisioning document is its configuration file
// (linphonerc) written as XML: a <config> root, <section name="..."> per
// configuration section and <entry name="..."> per key. The format and every
// key Render writes were checked against liblinphone's source on 2026-10-15,
// in linphone-sdk (https://gitlab.linphone.org/BC/public/linphone-sdk) at
// commit b5cd87b648f394126506145c3cc62b820f088fb8 (2026-10-14), where
// liblinphone now lives (the old liblinphone repository stopped at 5.4).
// Paths below are under liblinphone/:
//
//   - coreapi/xml2lpc.cpp, processEntry, processSection and processDoc
//     (lines 134-219): the root element must be "config" (its namespace is not checked), sections and
//     entries are matched by element name, an entry's value is its text, and
//     an existing key is replaced only when the entry has overwrite="true".
//     Every entry Render writes carries it, so a second scan (after a
//     reissue, or into a Linphone that already had an account) replaces the
//     old values instead of keeping them.
//   - share/xml/lpconfig.xsd: the namespace
//     http://www.linphone.org/xsds/lpconfig.xsd, which Linphone's own
//     examples put on <config>.
//   - coreapi/lpconfig.c, linphone_config_set_string (line 859): an empty
//     value removes the key. An empty entry with overwrite="true" is
//     therefore how the document deletes a key (misc/config-uri,
//     proxy_0/reg_route, and the codec keys below).
//   - coreapi/remote_provisioning.c, linphone_remote_provisioning_apply
//     (line 37): every [auth_info_N] section is deleted before a document is
//     applied, so auth_info_0 is exactly what the document says.
//   - src/auth-info/auth-info.cpp, AuthInfo(LpConfig*, key) (lines 105-120):
//     [auth_info_N] keys username, userid, passwd, ha1, realm, domain,
//     algorithm. fillBelleSipEvent (line 485) and coreapi/callbacks.c
//     fill_auth_info (line 886) answer a digest challenge with userid when it
//     is set, else username. That makes userid load-bearing: patchbay's
//     endpoints are identified by auth username, which is the device ID
//     (drivers.DeviceID), while the SIP user is the number. Without userid
//     Linphone would authenticate as "150", which names no endpoint.
//   - src/account/account-params.cpp, AccountParams(lc, index) (lines
//     246-288): [proxy_N] keys reg_identity, reg_proxy, reg_route, realm,
//     reg_expires, reg_sendregister, publish.
//   - coreapi/linphonecore.c line 2098 and coreapi/lpconfig.c lines 582-586:
//     sip/default_proxy picks the default account (and is set to 0 anyway
//     when a document adds proxy_0 and none was set).
//   - coreapi/remote_provisioning.c lines 222-242 and coreapi/linphonecore.c
//     lines 2894-2897 and 3777-3790: misc/config-uri is the provisioning URL
//     Linphone fetches at every start (linphone_core_start), and
//     misc/transient_provisioning=1 removes it after one successful apply.
//     The token in that URL is spent after one use, so a kept URL would be a
//     provisioning error at every launch (plan Review Focus 4). The document
//     does both: it deletes config-uri itself and sets
//     transient_provisioning, the documented switch for exactly this
//     (https://wiki.linphone.org/xwiki/wiki/public/view/Linphone/Remote%20Provisioning/).
//   - coreapi/linphonecore.c lines 2558-2565: video/capture, display,
//     automatically_initiate, automatically_accept.
//   - coreapi/linphonecore.c get_codec (line 2321) and codecs_config_read
//     (line 2456): [audio_codec_N] sections with mime, rate, channels,
//     enabled, recv_fmtp, bitrate, read from N=0 until the first section
//     without a mime; the same codec read twice keeps the last section's
//     enabled flag, and add_missing_supported_codecs (line 2393) adds every
//     other codec Linphone has after the listed ones, at its default. G.722
//     is disabled by default (line 2987). The document therefore lists the
//     profile's codecs (G722, then PCMU) as audio_codec_0.., enabled, and
//     deletes the next section's mime so an older list cannot follow and
//     disable them again. Their names and the 8000 Hz RTP clock rate (RFC
//     3551's, also G.722's) are ortp's (ortp/src/avprofile.c lines 116 and
//     772 at the same commit).
//   - coreapi/linphonecore.c lines 4330 and 4340: sip/use_info (default 0)
//     and sip/use_rfc2833 (default 1). The document sets both to the
//     defaults, so DTMF is RFC 4733 (Asterisk's dtmf_mode) whatever the
//     install had.
//
// The contacts keys were checked later, in linphone-sdk's GitHub mirror
// (https://github.com/BelledonneCommunications/linphone-sdk) at commit
// 79aefb33f61261095a1e8b1e4c9a5aafbcf8c642 (2026-10-26):
//
//   - coreapi/linphonecore.c, linphone_configuring_terminated (lines
//     2892-2949), which ends every start, with or without provisioning:
//     misc/contacts-vcard-list names a vCard 4.0 list that Linphone keeps as
//     a read-only friend list of its own (named by the URL), downloaded
//     again at every start while misc/fetch_contacts_vcard_list_at_startup
//     is 1 (its default, written anyway). The document names
//     compile.ContactsFile, so the PBX's directory shows as contacts; the
//     file's format is compile's (contacts.go).
//
// Only a real Linphone proves the document (docs/plans/release-a.md §8,
// "Does Linphone honour userid, misc/config-uri and the video keys?"); the
// e2e fake phone reads the same document, and downloads and parses the
// contacts it names. The house's first real Linphone took the document
// and registered on 2026-10-26, before the contacts keys existed.
//
// What the document leaves out, on purpose:
//   - the label. reg_identity carries no display name: what others see is the
//     endpoint's caller ID, which Compile sets from the label, and a label
//     change must not change this document, or every relabel would make the
//     softphone stale and ask its owner to scan a QR code again;
//   - the token: it is in the URL, never in the document;
//   - the password: only its placeholder, which the agent resolves.
//
// # The state machine
//
// Decide is the rule table of design §8.4 (Invalid, Unknown, Issued,
// Provisioned, Registered, Offline), rows S1 to S8 of the plan; see
// decide.go.
package softphone
