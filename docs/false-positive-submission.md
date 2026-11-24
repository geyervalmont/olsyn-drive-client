# Reporting a false positive to Microsoft

Microsoft Defender Antivirus flagged an unsigned pre-release of `olsyn-drive.exe` as **Trojan:Win32/Bearfoos.A!ml**.
The `!ml` suffix marks a detection by a machine-learning model rather than a known-malware signature. This page
is how a maintainer reports it to Microsoft as a false positive. Report each release that is detected; signing
the releases is the lasting fix.

## 1. Get the facts about the exact file

Use the file that was detected (or the release exe itself, unchanged; do not rebuild it, the hash would differ):

    make fp-info                       # dist/windows-amd64/olsyn-drive.exe
    make fp-info EXE=path/to/olsyn-drive.exe

It prints the file name, size, SHA-256, version and the version details (company, product, description,
copyright, original file name), and confirms the program runs as the invoking user (`asInvoker`). It fails when a
detail is missing. The site takes files up to 50 MB.

On the PC where it was detected, the security intelligence (definition) version is in PowerShell:

    (Get-MpComputerStatus).AntivirusSignatureVersion

## 2. Submit

At <https://www.microsoft.com/en-us/wdsi/filesubmission>:

1. **Submit file as a**: **Software developer**, then **Continue** (it asks you to sign in with a Microsoft account).
2. **Select the Microsoft security product used to scan the file**: Microsoft Defender Antivirus (Windows 11), or
   (Windows 10), as on the PC where it happened.
3. **Company name**: Olsyn.
4. **File**: the exe from step 1.
5. **What do you believe this file is?**: **Incorrectly detected as malware/malicious**.
6. **Detection name**: `Trojan:Win32/Bearfoos.A!ml`
7. **Definition version**: from step 1.
8. **Additional information** (English, at most 1,900 characters). Start with the product name and version from
   `make fp-info`, then this description:

   > Olsyn Drive is the desktop client for the Olsyn file drive: it shows the drive as a drive letter through
   > WinFsp and uploads and downloads files in the background, and it is open source under the Apache License 2.0
   > at https://github.com/geyervalmont/olsyn-drive-client. This pre-release build is not yet code-signed; it is
   > a Go program that installs for the current user only under %LOCALAPPDATA%\Programs\Olsyn Drive and runs
   > without administrator rights (only WinFsp's own signed installer, started with the person's consent, asks for
   > them); it talks to its own drive server and, for that installer, to the WinFsp project's GitHub release. We believe Trojan:Win32/Bearfoos.A!ml is a
   > machine-learning false positive and ask for the detection to be reviewed.

   Then add the SHA-256 and the size.

Do not change the program to get past the check (no packing, obfuscation or renaming); only report it.

## 3. Afterwards

**View submission history** on the same site shows the analyst's answer. When the detection is removed, a PC picks
it up with its next security intelligence update (Windows Security > Virus & threat protection > Protection updates
> Check for updates, or `Update-MpSignature` in PowerShell). Testers who added the folder exclusion described in
[pre-release-windows.md](pre-release-windows.md) can then remove it.
