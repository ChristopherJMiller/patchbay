# Phone messages: the Desk as the house's notice board

Release C, part 1 (patchbay 0.4.0). Written 2026-10-26 from a conversation
with the house's owner (Chris). Design §10 sketches push ("`patchbay phone
message desk "Dinner!"` → `POST /CGI/Execute`; the phone checks credentials
against `authenticationURL`, which the agent answers"); this document is
the part of release C that was asked for, with the decisions taken.

Calls marked **(builder's call)** were made while writing this, not by
Chris; everything else is his.

## Intent

When something in the house needs attention, or someone wants to say
"Dinner!", the text appears on the Cisco desk phone's screen with a short
beep so it gets noticed. The phone is a quiet notice board: it must never
interrupt a call, never make noise at night unless the message is urgent,
never tell an empty house, and never show old news.

Sources:
- **House events** from Home Assistant automations. First two: water leaks
  (`binary_sensor.water_heater_leak_sensor`,
  `binary_sensor.kitchen_sink_leak_sensor`) and the fridge or freezer door
  left open (`binary_sensor.fridge_door_contact`,
  `binary_sensor.freezer_door_contact`). Designed so more (laundry,
  dishwasher) are one automation each once sensors exist.
- **Typed messages**, from HA and from the patchbay CLI.

## Goals

1. **Text on the screen, with a beep first.** The screen shows **every
   current message, newest first**: each push re-renders the whole list,
   so a new message never wipes an unread one (an urgent leak stays
   visible under a later fridge note). A message is current from delivery
   until it expires or is withdrawn. When none is left, patchbay
   **clears the screen**. Exit closes the screen on the phone; the
   messages stay current in patchbay, so the next push shows them again
   until they expire.
2. **Hold-back rules.** A message waits while the phone is on a call,
   during quiet hours, and while nobody is home. **A call** is a
   person-to-person call, ringing included (builder's call: a push would
   cover the incoming-call screen). **A Stream call (radio, TV) does not
   hold messages**: they show, with the beep, while the stream plays.
3. **Held messages are delivered later unless stale.** When the reason
   clears (the call ends, quiet hours end, someone comes home), the held
   messages that have not expired are shown. A message has an expiry; the
   default is **1 hour** after it was sent. Expired messages are dropped,
   from the queue and from the screen.
4. **Urgent flag.** Any sender can mark a message urgent. Urgent skips
   quiet hours and "nobody home", but **never interrupts a call in
   progress**: it waits for the call to end, like any message.
5. **Withdrawing.** A message can carry a key (e.g. `fridge-door`). A newer
   message with the same key replaces the older one (no duplicates when a
   door keeps opening or a sensor flaps), and a sender can withdraw the
   message for a key: it leaves the queue, or the screen if delivered.
   **Fridge and freezer messages are withdrawn when the door closes. Leak
   messages are not withdrawn when the sensor dries**: a dried leak still
   needs a look, so a leak message stays until it expires. Future events
   choose per event; the default is not to withdraw.
6. **Each message shows the time it was sent** (builder's call), so
   nothing on the screen reads as newer than it is.
7. **Home Assistant hookup** in git (luma-homeops): a notify service HA
   automations and scripts can call, the two first events, a way to type a
   message in HA, and HA telling patchbay the house state.
   - The leak and fridge/freezer-ajar alerts **already exist** in
     `cluster/home-assistant/packages/home_alerts.yaml` (they push to the
     mobile phone; ajar means open 5 minutes). The Desk is added to those
     automations as a second action, not as new automations, and the
     fridge/freezer ones gain the withdraw on close. Leak is sent urgent,
     fridge/freezer not, as on the mobile alerts.
   - `hactl lint` rule `notify-outside-alerts`
     (`tools/hactl/src/hactl/lint.py`) rejects any `notify.*` call outside
     `home_alerts.yaml`: every Desk notification lives in that file.
8. **CLI**: `patchbay phone message <phone> "<text>" [--urgent]
   [--expires <duration>] [--key <key>]`, and a way to withdraw by key.

## Non-goals (this round)

- Spoken announcements (RTPRx / startMedia), the doorbell, Linphone chat
  (SIP MESSAGE), laundry and dishwasher (no sensors yet), the rest of
  release C (buttons, wallpaper, ringtones, phone-status Services pages).
- Remote (off-LAN) sending: everything stays inside the cluster and the
  house network.

## Anti-goals (a failure even if it works)

- A push that disturbs a call in progress, or a beep at night for anything
  not marked urgent.
- A message shown hours late as if it were new.
- A new port or service reachable from the LAN or the internet for this.
- A message or token that ends up in a log, an Event or status text in a
  form that lets someone else send to the phone.
- A broken or refused push that affects calls or the phone's registration.

## Decisions

### Who owns which rule

- **patchbay owns delivery and the queue**: it alone knows whether a phone
  is on a call, so it holds, releases, expires and withdraws messages. The
  agent's observations carry `Calls` per device (every AMI channel on the
  endpoint, `internal/agent/observe`), which counts Stream calls too: the
  observations must tell a Stream call from a person-to-person one, an
  agent API contract change.
- **Home Assistant owns quiet hours and presence**, and tells patchbay
  their state: "quiet on/off" and "away/home", over the same in-cluster
  connection as the messages. Quiet hours are **22:00–08:00**, an HA
  schedule in git (luma-homeops). Presence is
  `binary_sensor.house_occupied`. patchbay keeps the last state it was
  told, and that state survives a controller restart.
- **(builder's call)** With no state ever received, patchbay delivers (not
  quiet, someone home). HA sends its current state at HA start and on
  every change, so a lost update heals at the next restart or change.
  Expiry bounds the damage of a wrong state: nothing waits more than its
  expiry.

### How HA reaches patchbay (builder's call, approved as "your call")

- A **notify service in HA** (`notify.desk_phone`) that posts to a
  **token-protected patchbay endpoint inside the cluster**. HA runs on
  galaxy (ns `home-assistant`), so the endpoint is a ClusterIP Service in
  ns `telephony`; nothing new is reachable from the LAN. The same
  endpoint takes the house state (quiet, away).
- The token is a bearer token. Its value lives in one git-crypt +
  SealedSecret source in luma-homeops, sealed into both namespaces (HA
  reads it via `!secret`; the chart references the existing Secret by
  name). It is never logged.

### Messages are resources

- **(builder's call)** A message is a namespaced resource (e.g. `Message`)
  so it survives restarts and shows its outcome: `kubectl get messages`
  lists each with its target, state (Held with the reason, Delivered,
  Expired, Failed) and times. The endpoint and the CLI both create them;
  the controller is the only decider, as everywhere in patchbay.
  Delivered and expired messages are deleted after a retention period
  (builder picks; a day is fine).
- A message names its target phone(s) by Phone name. HA's
  `notify.desk_phone` targets `desk`. (builder's call: the API takes a
  list, so a second phone is a second notify service or a target list.)
- Held messages released together show on the one screen of current
  messages (goal 1), with a single beep.

### On the phone (Cisco 79x5, SIP 9.4(2)SR4-3)

Facts (docs/research/2026-10-14-cisco-7945-capabilities.md, "Push"):
`POST /CGI/Execute` with form field `XML` and Basic auth; the phone checks
the credentials against `authenticationURL`, which must answer
`AUTHORIZED`; an Execute holds at most 3 items; a pushed object is at most
512 bytes, so larger content is an Execute pointing at a URL; XML is
served as `text/xml`; `Play:` is a supported URI.

- The SEP file gains `authenticationURL` (device HTTP :8088, answered by
  the agent) and `phonePersonalization` turns on (it is 0 today precisely
  because nothing authenticated pushes). This changes every Cisco phone's
  config version: each resyncs once, never during a call.
- The push credentials are generated by patchbay (Secret, placeholders
  as usual); only the agent knows them and answers the phone's check.
- The push itself goes from the PBX node (.240). The agent already reaches
  each phone's web server on port 80 to scrape it, so no router change is
  expected; the builder confirms this against
  `router/ansible/playbooks/firewall.yml` before relying on it.
- The text is a `CiscoIPPhoneText` (title, the current messages newest
  first with their times, an Exit softkey), served from device HTTP and
  pushed as an Execute pointing at it. The beep is an Execute `Play:` item
  ahead of it (builder picks the sound and where the phone fetches it
  from); only a new message beeps, not a re-render for an expiry or a
  withdraw. Clearing the screen is a second kind of push (builder picks
  how; the bench confirms it).
- Push support is per model in the driver's profile
  (`FeaturePushNotification`, Untested today). A phone whose push is
  refused shows it in its status; calls are unaffected.

### If the phone refuses pushes: the idle-screen fallback

Chris chose a fallback over shipping push as unsupported. If bench
question 1 is a no, the phone **pulls** the messages instead: its SEP
`idleURL` points at the same current-messages page on device HTTP, shown
after `idleTimeout` of inactivity. Text shows, with no beep. The hold-back,
expiry and withdraw rules are the same; only delivery differs. The plan
carries both branches and **starts with a small bench build** that
answers question 1 on the real Desk before either branch is built in
full.

### Bench questions (the real 7945 answers these; plan for each outcome)

1. Does SIP 9.4(2)SR4-3 accept an authenticated `/CGI/Execute` push and
   check `authenticationURL` as documented? (No: the idle-screen
   fallback.)
2. Does `Play:` make a sound, from which file location, and what does it
   do to a Stream call's audio that is playing?
3. Does a push wake the screen when display power-save has turned it off
   (the Desk goes dark after 1 minute idle)? If not, the beep is the only
   cue, or the push must wake it first. (Fallback: does the idle URL show
   while the screen is dark?)
4. Does a second push replace the shown text, and which push clears the
   screen back to the phone's idle display?

## Constraints

- patchbay's rules hold: pure Compile/Render/Decide with golden and
  rule-table tests; only the controller decides and writes status; secrets
  only as placeholders in compiled files; the e2e fake phone grows what it
  needs (a `/CGI/Execute` that checks `authenticationURL` like the real
  phone) and imports nothing of patchbay's.
- Contracts change deliberately (CLAUDE.md lists them): a new resource,
  an agent action, SEP fields, an endpoint.
- Local builds and tests on 4 threads; kind e2e and asterisk-check run in
  GitHub CI.
- luma-homeops: HA config is git (`cluster/home-assistant/`, the
  `ha-config` skill and `hactl`), secrets via git-crypt + `./sign.sh`,
  Argo applies.
- Ship as patchbay 0.4.0, then the galaxy bump; the Desk resyncs once.

## Left to the builder

The endpoint's wire format and port; the resource's exact name, fields and
status; how presence and quiet state are stored; retention; the beep sound;
the text layout and title; retry and backoff for a failed push; the HA YAML
(notify platform, automations, the typed-message card); how the fake phone
models the real one; metrics and Events.
