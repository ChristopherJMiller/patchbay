# Cisco CP-7945G SIP firmware: how to get it, and what to do with it

Researched 2026-10-14 for patchbay. Nothing was downloaded. Facts come with
sources; where something could not be verified, it says so.

## Bottom line

- **A free Cisco.com account is the only legitimate route, and it may no
  longer work.**
- **A service contract can't be bought.** Cisco EOL12104 (7945/7965) lists:

  | Milestone | Date |
  |---|---|
  | End of contract renewal | 2022-09-13 |
  | Last date of support | 2023-06-30 |

  Cisco's relicensing page excludes secondary-market gear from contracts
  anyway.
- **A free account used to be enough.** In 2019 it downloaded the 794x/796x
  SIP 9.4(2)SR3 zip without a contract
  (https://github.com/waynemerricks/Cisco7942FreePBX). Locale and `.tlv` files
  did need one.
- **SR4 may always have been gated.** A 2021 Cisco Community post says SR4 was
  "only through Cisco partners".
- **Current state is unknown.** Since end of support the 7900 pages show only
  "Log in to see available downloads".

**Realistic path:**
1. Make a free account and look under Unified IP Phone 7965G → Session
   Initiation Protocol (SIP) Software.
2. If `cmterm-7945_7965-sip.9-4-2SR4-3.zip` downloads (or `…SR3-1.zip`, if SR4
   is gated), use it and record the MD5/SHA512 Cisco shows.
3. Otherwise there is no Cisco-sanctioned source.

## Target loads

- **9.4(2)SR4-3**, load ID `SIP45.9-4-2SR4-3S`, listed in Cisco's CUCM Device
  Pack 14.0(1.15058-2) notes, Aug 2024:
  https://www.cisco.com/c/en/us/td/docs/voice_ip_comm/cucm/device_packs/1401-devpack-for-aug-2024.html

  | File | Size (bytes) |
  |---|---|
  | `SIP45.9-4-2SR4-3S.loads` | 673 |
  | `apps45.9-4-2SR4-3.sbn` | 4,637,904 |
  | `cnu45.9-4-2SR4-3.sbn` | 582,170 |
  | `cvm45sip.9-4-2SR4-3.sbn` | 2,693,976 |
  | `dsp45.9-4-2SR4-3.sbn` | 364,907 |
  | `jar45sip.9-4-2SR4-3.sbn` | 2,260,034 |
  | `term45.default.loads` | 673 |
  | `term65.default.loads` | 673 |

  Total about 10.5 MB. Source:
  https://goughlui.com/2022/12/18/project-connecting-cisco-ip-phone-7945-cp-7945g-with-freepbx/

- **9.4(2)SR3-1**, the fallback. The `.sbn` files carry "ES26" names: apps45,
  cnu45, cvm45sip, dsp45 and jar45sip `.9-4-2ES26.sbn`. The zip is
  6,512,565 bytes.
- **Checksums.** No Cisco-published hash was found in public; Cisco shows them
  after login. The driver manifest pins the SHA-256s of the files from
  Cisco's own zip.

## Upgrade path from SCCP 9.4(2)SR4-3

- **Direct; no intermediate load.** Cisco documents an intermediate step only
  for phones below 8.3(3).
- **Hardware revision ≥ 13** can't run anything below 9.3(1)SR3. That doesn't
  affect 9.4(2).
- **The trigger** is `<loadInformation>SIP45.9-4-2SR4-3S</loadInformation>` in
  `SEP<MAC>.cnf.xml`, with no `.loads` extension.
- **After a factory reset** the phone requests `term45.default.loads`. Serve
  the **SIP** zip's copy.
- **Factory reset:** hold `#` at power-on, then `123456789*0#`. It erases the
  phone application, the CTL/LSC files and the network config. Keep the SIP
  files on TFTP first.
- **Ex-CUCM trust lists:** Settings → Security Configuration → Trust List
  (`**#`) → ITL → Erase, or a factory reset.
- **Reported failures:**
  - "Load Authentication Failed" upgrade loops, blamed on a bad SR4 copy and
    fixed by going SR3 → SR4:
    https://www.mail-archive.com/cisco-voip@puck.nether.net/msg15029.html
  - Trust-list config verification failures.

## Integrity

All 7945/7965 loads are signed, and phones only accept signed releases:
https://www.cisco.com/web/software/282074288/54796/cmterm-7945_7965-sip.9-2-1SR2-readme.html

The bypass CVE-2011-1637 was fixed in 9.2.1. So a tampered image fails with
"Load Authentication Failed". A mirror could still serve a genuine *older*
load, and the config files are unsigned.

## Licensing

Cisco's EULA licenses software only from an "Approved Source" and forbids
redistribution. Its Software Transfer and Re-Licensing Policy (v3.3) doesn't
transfer use rights to a surplus buyer without Cisco's consent and a fee.
patchbay documents only the Cisco route above (a Cisco.com account), and
names no unofficial source.

**Storage.** git-crypt in a public repo still publishes the ciphertext, the
paths and the sizes, permanently. patchbay keeps the files on a PVC and commits
only a manifest of names and SHA-256s.
