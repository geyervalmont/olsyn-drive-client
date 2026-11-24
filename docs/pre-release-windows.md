# Olsyn Drive pre-release for Windows: read this first

This is an early, **unsigned** test version of Olsyn Drive. It is not signed with a code-signing certificate
yet, so Windows does not know who made it and may warn you or remove it. Nothing is wrong with your PC and
nothing is lost if that happens. The signed release will not need any of the steps below.

## The easy way: the install wizard

A short guided installer runs in PowerShell. It explains each step, creates Olsyn Drive's program folder, copies
its path for you, opens Windows Security on the right page, checks your work, installs Olsyn Drive and opens it.
It never changes Windows Security itself: you make the one change, by hand, and it shows you where.

**For now (test builds sent to you directly).** Download the Olsyn Drive file you were sent (`olsyn-drive.exe`)
and `install.ps1`, both into your Downloads folder. Then:

1. Open the Start menu, type **PowerShell**, right-click **Windows PowerShell** and choose **Run as administrator**,
   then **Yes**. (Administrator lets the wizard check the Windows Security step for you. Without it, the wizard
   works too and asks you to confirm instead.)
2. Paste this line and press Enter:

       & ([scriptblock]::Create((Get-Content -Raw "$HOME\Downloads\install.ps1"))) -PreRelease -Installer "$HOME\Downloads\olsyn-drive.exe"

   Or, without downloading `install.ps1`:

       & ([scriptblock]::Create((irm https://drive.olsyn.com/install.ps1))) -PreRelease -Installer "$HOME\Downloads\olsyn-drive.exe"

   If you were sent a SHA-256 with the file, add `-Sha256 <that value>` at the end and the wizard refuses a file
   that does not match; without it, it prints the file's SHA-256 so you can compare it yourself.
3. Follow the five steps on screen. Type Q at a question to stop without installing anything.

**Once releases are published on the drive**, the same wizard starts by itself for a test build. Open PowerShell
(as above) and run:

    irm https://drive.olsyn.com/install.ps1 | iex

To see the steps without installing anything, add `-DryRun`. To check the wizard itself on a PC,
`powershell -File install.ps1 -SelfTest` prints its checks and ends with "Self-test passed".

Doing it by hand instead, or if the wizard cannot run: the steps below are the same ones.

## What Windows may show

- **Your browser** may say the file isn't commonly downloaded. Choose to keep it (in Edge: the "..." menu next
  to the download, then **Keep**, then **Show more**, then **Keep anyway**).
- **"Windows protected your PC"** (SmartScreen) when you open it: choose **More info**, then **Run anyway**.
- **Windows Security may remove it.** In our own testing, Microsoft Defender flagged the unsigned test build as
  "Trojan:Win32/Bearfoos.A!ml" a couple of minutes into a copy and removed it. That name is a guess made by an
  automatic (machine-learning) check, not a known virus, and we believe it is a false alarm. To stop it
  happening, allow Olsyn Drive's program folder as below.

## Allow Olsyn Drive's program folder by hand (once)

Allow **only this folder**:

    %LOCALAPPDATA%\Programs\Olsyn Drive

Never add the cache or data folder, the W: drive, your Downloads folder or a whole disk.

1. Open **Windows Security** (search for it in the Start menu).
2. Select **Virus & threat protection**.
3. Under "Virus & threat protection settings", select **Manage settings**.
4. Scroll to **Exclusions** and select **Add or remove exclusions**. Choose **Yes** when Windows asks for permission
   (this one step needs an administrator on the PC).
5. Select **Add an exclusion**, then **Folder**.
6. In the folder window, click the address bar at the top, paste `%LOCALAPPDATA%\Programs` and press Enter.
   If there is no **Olsyn Drive** folder yet, create it with **New folder** and name it exactly `Olsyn Drive`.
7. Select the **Olsyn Drive** folder and choose **Select Folder**.

Then open the downloaded file and choose **Install**.

Another way, after a detection: **Windows Security > Virus & threat protection > Protection history**, select the
Olsyn Drive detection, then **Actions > Allow on device**. This is broader: it allows that detection wherever
it appears, not just in one folder. The folder exclusion above is the better choice.

## If it was already removed

- **Windows Security > Virus & threat protection > Protection history**, select the Olsyn Drive item, then
  **Actions > Restore**. Or simply add the folder exclusion above and download Olsyn Drive again.
- Your files waiting to upload are **safe**. They are kept in `%LOCALAPPDATA%\Olsyn Drive` (not in the program
  folder) and carry on uploading by themselves once Olsyn Drive is installed again. The installer says
  "Reinstall" when it finds them. Do not delete that folder.
- If Olsyn Drive tells you "Windows Security removed or blocked Olsyn Drive on this PC", this is what it means.

## Removing the exclusion later

When you install the signed release (or if you stop using Olsyn Drive), remove the exclusion:
**Windows Security > Virus & threat protection > Manage settings > Exclusions > Add or remove exclusions**, select
the Olsyn Drive folder and choose **Remove**.

## Uninstalling

**Settings > Apps > Installed apps**, find **Olsyn Drive**, select "..." and **Uninstall**. It asks before deleting
anything that has not reached the drive yet. Then remove the exclusion as above.
