# Cisco CP-7945G on SIP firmware 9.4(2), driven by Asterisk 22

Researched 2026-10-14 for patchbay. Each claim is tagged with its source type:
- **(doc)** Cisco or usecallmanager documentation;
- **(src)** code read directly;
- **(community)** a forum or blog report;
- **(UNSURE)** inferred or conflicting, so bench-test it.

The bench tests are listed in spec §8.6.

## The short version

Under SIP 9.4(2), with `<proxy>USECALLMANAGER</proxy>`, the phone runs in
"CUCM mode". In that mode these features all use Cisco-proprietary SIP:
- BLF;
- the Conference, Park, iDivert and Record softkeys;
- DND and call-forward sync;
- auto-answer;
- multi-line registration.

The proprietary SIP is out-of-dialog REFER with `x-cisco-remotecc` bodies,
PUBLISH, and Cisco PIDF. Only the usecallmanager.nz patch implements it, and
for Asterisk 22 that patch ships a re-added `chan_sip` marked "ready for
testing". patchbay stays on stock PJSIP and records the gaps per backend.

## 1. Hardware

Sources: the Cisco data sheet C78-418622 and the 9.0 admin guide.

**Display**
- 5" TFT, 320×240, 16-bit colour.
- XML service area 298×156; 320×156 in `WindowMode="Wide"` (XSI SDK 9.1(1),
  Table 3-2).
- Wallpaper 320×212×16 PNG, with an 80×53 thumbnail.

**Keys**
- 2 illuminated line/feature keys, 4 softkeys plus "more", and a 4-way
  navigation cluster.
- Fixed keys: Messages, Directories, Help, Settings, Services, Speaker, Mute,
  Headset, the volume rocker, and Display.
- No KEM.

**Network and power**
- 2-port 10/100/1000 switch with 802.1Q/p.
- 802.3af Class 3.

**Audio**
- Wideband (G.722) on handset, headset and speaker.
- Codecs: G.711a/µ, G.722, G.729a/ab, iLBC, L16. iLBC and L16 against Asterisk
  are UNSURE.
- EHS through `ehookEnable`.

**Other**
- SSL VPN client (ocserv ≥ 1.2.0).
- 802.1X: EAP-MD5, EAP-FAST, EAP-TLS.

## 2. Provisioning

**Boot sequence**
1. CDP/LLDP voice-VLAN learning.
2. DHCP (option 150 preferred over 66).
3. Possibly **HTTP on tcp 6970 first**, falling back to TFTP after a timeout
   (UNSURE per build).
4. `CTLSEP<MAC>.tlv` / `ITLSEP<MAC>.tlv` / `ITLFile.tlv`.
5. `SEP<MAC>.cnf.xml`. A phone holding a trust list asks for `.sgn` first.
6. `XmlDefault.cnf.xml`, only as a fallback.
7. Firmware, but only if `loadInformation` differs from what's running.
8. The dial template, softkey file and locales.
9. REGISTER.

**Key `SEP<MAC>.cnf.xml` elements**
(https://usecallmanager.nz/sepmac-cnf-xml.html,
https://github.com/usecallmanagernz/tftpboot)

- **Time.** `dateTimeSetting` uses a Windows-style `timeZone`, e.g.
  "Pacific Standard/Daylight Time". The NTP server must be **an IP address**.
- **Registrar.** `callManagerGroup` members (`processNodeName`, `sipPort`)
  are the registrar when lines use USECALLMANAGER.
- **`sipProfile`:**
  - `registerWithProxy=true`;
  - `sipCallFeatures`, with the `x-cisco-serviceuri-*` URIs;
  - `remotePartyID`;
  - `dtmfAvtPayload=101`;
  - `phoneLabel`: **≤ 12 characters, no spaces**;
  - media ports, default 16384–32766;
  - the `dialTemplate` and `softKeyFile` names.
- **`sipLines/line`:**
  - `featureID` and `featureLabel` (≤ 30 characters);
  - `proxy`, which **must be USECALLMANAGER** on 9.x;
  - `name`, `displayName`, `authName`, `authPassword`;
  - `autoAnswer`, `callWaiting`, `messageWaitingLampPolicy`, `maxNumCalls`
    (up to 50).
- **`featureID` values:**

  | ID | Meaning | ID | Meaning |
  |---|---|---|---|
  | 1 | Redial | 2 | Speed dial |
  | 3 | Hold | 4 | Transfer |
  | 5 | CFwdAll | 9 | **Line** |
  | 20 | Service URL | 21 | **BLF** |
  | 22 | BLF park | 23 | Intercom |
  | 126 | Park | 127 | Pickup |
  | 130 | DND | 194 | Messages |
  | 195 | Directories | | |

  The full list is at https://usecallmanager.nz/line-keys.html.
- **`transportLayerProtocol`.** 1 = TCP (**use it**; UDP gives retransmit
  errors), 2 = UDP, 3 = TLS.
- **`vendorConfig`:**
  - `webAccess` and `sshAccess` (**0 = enabled**);
  - `settingsAccess`, `pcPort`, `voiceVlanAccess`;
  - CDP/LLDP per port;
  - G.722 and wideband toggles;
  - `ehookEnable`.
- **Top-level elements:**
  - `loadInformation` (no `.loads`; blank means keep);
  - `versionStamp` (bump on every change);
  - `userLocale` / `networkLocale`;
  - `directoryURL`, `servicesURL`, `messagesURL`, `informationURL`,
    `idleURL`/`idleTimeout`, `authenticationURL`, `proxyServerURL` (each ≤ 255
    characters);
  - `advertiseG722Codec`, `phonePersonalization`;
  - `sshUserId` / `sshPassword`.

**Other files**
- **`dialplan.xml`.** `<DIALTEMPLATE><TEMPLATE MATCH Timeout [Rewrite Line
  Tone]/>`. `.` is one digit, `*` is zero or more. With no template the phone
  waits 15 s (https://usecallmanager.nz/dial-template.html).
- **Softkey file.** One `softKeySet` per call state
  (https://usecallmanager.nz/soft-keys.html).
- **Ringtones.** `Ringlist.xml` (`CiscoIPPhoneRingList`, ≤ 50 entries). Each
  file is raw headerless µ-law, 8 kHz, 8-bit, mono, 240–16080 samples:
  `sox in.wav -t raw -e u-law -b 8 -c 1 -r 8000 out.raw`. Some phones request
  `RINGLIST.XML` or `DistinctiveRingList.xml` instead (UNSURE; read the TFTP
  log).
- **Backgrounds.** `Desktops/320x212x16/List.xml` (`CiscoIPPhoneImageList`).
- **Locales.** `be-sip.jar` and `g3-tones.xml`, not needed for en_US.

**What takes effect when**
- **Changes to the SEP file, dial template or softkey file** need a restart.
  CUCM's "Apply Config" is a `NOTIFY Event: service-control` with body
  `action=restart` (re-read) or `action=reset` (reboot), plus
  `RegisterCallId={<REGISTER Call-ID>}` (src: usecallmanager `sip_notify`
  samples). Stock `PJSIPNotify` with a custom body should be able to send it
  (UNSURE).
- **Live:** XML services, MWI and BLF state, and the ring/wallpaper lists.

## 3. XML Services (SDK 9.1(1), Tables 3-1 / 5-1 / 5-2)

**Objects.** All are supported on 79x5: Menu, Text, Input, Directory, Image,
ImageFile, GraphicMenu, GraphicFileMenu, IconMenu, IconFileMenu, Status,
StatusFile, Execute, Response, Error.

**Limits**
- At most 100 menu items.
- A pushed object is at most 512 bytes; anything larger goes as an Execute
  pointing at a URL.
- Content must be served as `text/xml`.

**URIs**
- Supported on 79x5: `Key:`, `SoftKey:`, `Init:`, `Dial:`/`EditDial:`,
  `Play:`, `QueryStringParam:`, `RTPRx`/`RTPTx`, `RTPMRx`/`RTPMTx`, `Notify:`,
  `SendDigits:`, `App:`.
- Not supported: `Display:` (7970/75 only), `Vibrate:`, `Device:`.
- `Key:` doesn't support NavDwn, NavLeft, NavRight, NavSelect, Offhook or
  Onhook on this column.

**Push**
- `POST /CGI/Execute` with a form field `XML` and Basic auth. The phone checks
  the credentials against `authenticationURL`, which must answer `AUTHORIZED`
  (https://usecallmanager.nz/cgi-execute.html).
- Execute carries ≤ 3 items, with Priority 0, 1 or 2.

**Other**
- `/CGI/Screenshot` needs Basic auth.
- Request headers from the phone: `x-CiscoIPPhoneModelName`,
  `x-CiscoIPPhoneDisplay`, `x-CiscoIPPhoneSDKVersion`.

**RTPRx**
- Syntax: `RTPRx:ip:port[:vol]`, `RTPMRx:mcast:port[:vol]`, `RTPRx:Stop`.
- Format: G.711µ, 20 ms packets, even port 20480–32768.
- Documented as supported on 79x5, along with the newer `startMedia` API.
- A 2008 report says early 79x5 firmware ignored it. **Bench-test it on SIP
  9.4(2).**

## 4. Features against Asterisk 22

| Feature | Stock PJSIP | usecallmanager patched chan_sip |
|---|---|---|
| Registration | Works with USECALLMANAGER, TCP, **`force_rport=no` and `rewrite_contact=no`**. Keep passwords short: 32 characters fails, ≤ 8–12 is known good (community, UNSURE on the limit). | Yes |
| Multiple lines | UNSURE. CUCM mode REGISTERs line 1 only. | Bulk registration |
| BLF | **No.** The phone asks for `cpim-pidf+xml`, and PJSIP answers with XPIDF (ASTERISK-28603 unmerged). | Yes |
| MWI | Probably works with unsolicited NOTIFY (UNSURE) | Yes |
| Hold | Yes | Yes |
| Transfer | Probably yes, via in-dialog REFER (UNSURE; bench) | Yes |
| Conference / Park softkeys | **No** (remotecc REFER) | Yes |
| Pickup, MeetMe, AbbrDial | **Yes, via dialplan** (INVITE to `x-cisco-serviceuri-*`) | Yes |
| CFwd / DND | Local only; not synced | Synced |
| Auto-answer intercom | UNSURE. Paging via RTPRx needs no call. | Yes |
| Distinctive ring | `Alert-Info: <Bellcore-dr1..5>` | Yes |
| Caller ID | Remote-Party-ID (`remotePartyID=true`, `send_rpid=yes`) | Yes |
| G.722 | Yes | Yes |
| DTMF | RFC 4733, payload 101 | Yes |
| Restart / reset from PBX | service-control NOTIFY (UNSURE on stock) | Yes |

## 5. Prior art

**usecallmanager.nz (Gareth Palmer)**
- Patches for Asterisk 20 (stable) and 22 ("ready for testing", 22.11.0,
  2026-09-13; a modernised `chan_sip`). They exist only for exact upstream
  versions.
- Tools: `tftpboot` templates, `services` (a Flask XML-services app),
  `commands` (cgiexecute, rtptransmit, mediastream, screenshot), and
  `certutils`.

**Declarative provisioners.** None is a k8s operator or Helm chart.
- The closest in spirit is NUR shelvacu `pbxvm/tftp.nix` (Nix-generated SEP
  files).
- Also: hskrk-ansible, Asterisk-Phone-Manager, Cisco-Provisioning-Server,
  cisco-asterisk-templates, CNF-Generator, buba0's tutorial.
- Wazo provd doesn't cover 79xx SIP.

## 6. Network and management

**VLAN learning**
- The phone learns its Operational VLAN from CDP or LLDP-MED. **Admin VLAN is
  used only if the switch advertises nothing.**
- On an untagged access port, make sure there's no LLDP-MED voice VLAN and no
  stale Admin VLAN.
- `pcPort=1` disables the PC port.

**Web UI** (read-only, HTTP, no IPv6), with XML twins:
- `/DeviceInformationX`: loads, hardware revision, serial.
- `/NetworkConfigurationX`: VLANs, TFTP, and "Unified CM n: Active".
- `/DeviceLogX`: status messages.
- `/StreamingStatisticsX`: per-call RTP stats.
- Only `/CGI/Execute` and `/CGI/Screenshot` are documented as needing auth.

**Observed 2026-10-15 on a CP-7945G hw rev 13, SIP 9.4(2)SR4-3** (the
bench phone, registered to patchbay; synthetic fixtures with this structure and invented values are in
`internal/agent/scrape/testdata/cp7945g-sip-9.4.2sr4-3`):
- `/DeviceInformationX` is well-formed XML (`utf-8`, CRLF after the
  declaration), root `<DeviceInformation>`. **The load ID is `versionID`,
  wrapped in asterisks:** `*SIP45.9-4-2SR4-3S*`. `appLoadID` names the jar
  image, `jar45sip.9-4-2SR4-3.sbn`; `bootLoadID` is `tnp45.9-3-1-CR17.bin`;
  `hardwareRevision` `13.0`; `modelNumber` `CP-7945G`. `udi` spans several
  lines (model text, `CP-7945G`, `V14`, the serial).
- `/NetworkConfigurationX` is **not** well-formed, every time: the phone's
  own Java renderer throws, and the page ends
  `<EnergywisePowerLevel>java.lang.NullPointerException` with no closing
  tags (3076 bytes). Everything patchbay reads comes before the break:
  `AdminVLANId` (empty), `CallManager1` (`192.168.0.240   Active`, three
  spaces), `TFTPServer1`, `DHCPServer`.
- `/DeviceLogX` answers 200 with an empty body.
- Element names are the first of each pair patchbay accepts
  (`AdminVLANId`, `CallManager1`); roots carry no trailing `X`.

**Other management**
- **SSH:** the configured user, then `debug`/`debug`. It uses legacy key
  exchange.
- **Syslog:** effectively none.
- **After a reboot,** the first REGISTER carries
  `Reason: SIP;cause=200;text="cisco-alarm:… Last=<why>"`.

## 7. Gotchas

**Trust lists**
- An ITL from the old CUCM makes the phone accept only configs signed by that
  CUCM. Erase the ITL or factory reset.
- If you ever issue your own ITL, losing its key means erasing every phone by
  hand.

**Factory reset**
- It **erases the phone application** and boots `term45.default.loads`, so
  the files must already be on TFTP or the phone hangs.
- Power-cycling mid-upgrade is the classic brick.

**Firmware**
- A `loadInformation` that names a missing file set causes retry loops.
- "Load Rejected HC" means hardware rev ≥ 13 can't run anything below
  9.3(1)SR3.

**Files and transport**
- TFTP file names are case-sensitive. Use `SEP` + **uppercase MAC**.
- If tcp 6970 is *dropped*, every file waits out a timeout. Allow it or reject
  it.
- TLS is **1.0 only**, so HTTPS services need `@SECLEVEL=0`.

**Stability**
- Reports of SIP firmware locking up after about 2 days need a power cycle.

## Modelling implications (as adopted in spec §4 and §8)

- **The capability profile is Go data inside the driver,** not a CRD. It holds
  the hardware facts and a feature table for each signalling backend.
- **`Phone` holds the per-device fields:**
  - MAC, model, firmware (nullable);
  - `buttons[]` as a typed union (release C);
  - lines, with a credential reference;
  - time zone, NTP, locale;
  - service URLs;
  - ring and wallpaper choices;
  - network and access policy.
- **The trust-list policy stays `none`.** A managed ITL is irreversible
  without manual erasure, so it isn't offered.
- **Generated as side effects:** `versionStamp`, a "needs restart" flag (→ the
  service-control NOTIFY), and status scraped from the phone's XML pages.
