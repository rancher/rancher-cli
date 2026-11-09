# rancher CLI installer for Windows. Windows PowerShell 5.1 and PowerShell 7.
# Canonical invocation: README "Install".
#
# Every statement lives inside Install-RancherCli and the final line is the
# only call, so a truncated download either fails to parse or defines a
# function nobody calls - it never runs a partial install.
param(
    [string]$Version = $env:RANCHER_CLI_VERSION,
    [switch]$Pre,
    [string]$BinDir = $env:RANCHER_CLI_INSTALL_BIN_DIR,
    [switch]$VerifyAttestation,
    [switch]$DryRun,
    [switch]$Quiet
)

function Install-RancherCli {
    param(
        [string]$Version,
        [switch]$Pre,
        [string]$BinDir,
        [switch]$VerifyAttestation,
        [switch]$DryRun,
        [switch]$Quiet
    )

    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $Repo = 'rancher/rancher-cli'
    $TagRe = '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$'
    $CliName = 'rancher.exe'
    $HelperName = 'rancher-credential-vault.exe'
    $Os = 'windows'

    # Mutable state shared by the phase functions below. Nested functions
    # cannot assign their caller's variables, so everything a phase sets for
    # a later phase lives in this table.
    $S = @{
        BaseUrl          = 'https://github.com'
        ApiUrl           = 'https://api.github.com'
        Token            = $env:GH_TOKEN
        UsedGhToken      = $false
        BinDir           = $BinDir
        DataDir          = $null
        ConfigDir        = $null
        VersionsDir      = $null
        Arch             = $null
        Tag              = $null
        CliAsset         = $null
        HelperAsset      = $null
        CliAssetId       = $null
        HelperAssetId    = $null
        SumsAssetId      = $null
        MetadataFailCode = $null
        ReleaseJson      = $null
        Http             = $null
        Work             = $null
        Lock             = $null
        StagingDir       = $null
        StagingLink      = $null
        VDir             = $null
        CurrentTarget    = $null
        Aside            = $null
        Sums             = $null
    }
    if (-not $S.Token) { $S.Token = $env:GITHUB_TOKEN }
    if ($env:RANCHER_CLI_INSTALL_BASE_URL) {
        $S.BaseUrl = $env:RANCHER_CLI_INSTALL_BASE_URL.TrimEnd('/')
        $S.ApiUrl = $S.BaseUrl
    }

    function Write-Note([string]$Message) {
        if (-not $Quiet) { [Console]::Out.WriteLine($Message) }
    }
    function Write-Warn([string]$Message) {
        [Console]::Error.WriteLine("warning: $Message")
    }
    function Fail([string]$Message) {
        throw $Message
    }

    # Native helpers: an atomic directory replace (one NTFS rename with POSIX
    # semantics - Rename-Item and MoveFileEx refuse to replace a directory,
    # junctions included), a create-or-fail directory for the install lock,
    # and a certificate pin for a server the OS does not trust. Compiled once
    # per session.
    function Import-Native {
        if ('RancherCliInstall.Native' -as [type]) { return }
        $source = @'
using System;
using System.IO;
using System.Net.Http;
using System.Net.Security;
using System.Runtime.InteropServices;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using Microsoft.Win32.SafeHandles;

namespace RancherCliInstall
{
    public static class Native
    {
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern SafeFileHandle CreateFileW(string name, uint access, uint share, IntPtr sa, uint disposition, uint flags, IntPtr template);

        [DllImport("kernel32.dll", SetLastError = true)]
        static extern bool SetFileInformationByHandle(SafeFileHandle handle, int infoClass, IntPtr info, int size);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern bool CreateDirectoryW(string path, IntPtr sa);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern int GetFinalPathNameByHandleW(SafeFileHandle handle, StringBuilder path, int size, int flags);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern int GetFullPathNameW(string name, int size, StringBuilder buffer, IntPtr filePart);

        const uint DELETE = 0x00010000;
        const uint FILE_SHARE_ALL = 0x7;
        const uint OPEN_EXISTING = 3;
        const uint FILE_FLAG_BACKUP_SEMANTICS = 0x02000000;
        const uint FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000;
        const int FileRenameInfoEx = 22;
        const int FILE_RENAME_FLAG_REPLACE_IF_EXISTS = 0x1;
        const int FILE_RENAME_FLAG_POSIX_SEMANTICS = 0x2;
        const int ERROR_ALREADY_EXISTS = 183;
        const int ERROR_NOT_SUPPORTED = 50;
        const int ERROR_INVALID_PARAMETER = 87;

        // ReplaceDirectory renames source over destination in one operation,
        // replacing an existing destination directory or junction. The
        // reparse point itself is renamed, never its target.
        public static void ReplaceDirectory(string source, string destination)
        {
            using (SafeFileHandle h = CreateFileW(source, DELETE, FILE_SHARE_ALL, IntPtr.Zero, OPEN_EXISTING,
                FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT, IntPtr.Zero))
            {
                if (h.IsInvalid)
                {
                    throw new IOException("cannot open " + source + " (error " + Marshal.GetLastWin32Error() + ")");
                }
                int nameBytes = destination.Length * 2;
                int size = 20 + nameBytes + 2;
                IntPtr p = Marshal.AllocHGlobal(size);
                try
                {
                    for (int i = 0; i < size; i++) { Marshal.WriteByte(p, i, 0); }
                    Marshal.WriteInt32(p, 0, FILE_RENAME_FLAG_REPLACE_IF_EXISTS | FILE_RENAME_FLAG_POSIX_SEMANTICS);
                    Marshal.WriteInt32(p, 16, nameBytes);
                    Marshal.Copy(destination.ToCharArray(), 0, new IntPtr(p.ToInt64() + 20), destination.Length);
                    if (!SetFileInformationByHandle(h, FileRenameInfoEx, p, size))
                    {
                        int err = Marshal.GetLastWin32Error();
                        // FileRenameInfoEx with POSIX semantics needs Windows 10
                        // version 1607 or Windows Server 2016; an older kernel
                        // rejects the info class or the flag with one of these
                        // two codes. Failing here is safe: the previous bin dir
                        // is still intact, and a non-atomic fallback would break
                        // the installer's kill-safety.
                        if (err == ERROR_INVALID_PARAMETER || err == ERROR_NOT_SUPPORTED)
                        {
                            throw new IOException("this OS lacks support for the atomic directory rename the installer requires (error " + err + ") - it needs Windows 10 version 1607 or Windows Server 2016 or later");
                        }
                        throw new IOException("cannot rename " + source + " over " + destination + " (error " + err + ")");
                    }
                }
                finally
                {
                    Marshal.FreeHGlobal(p);
                }
            }
        }

        // FinalPath returns the filesystem's own final long-form name for an
        // existing path, resolved through any reparse points: environment
        // variables can hand the installer 8.3 short names (RUNNER~1) whose
        // string form never equals the long name the same directory reports
        // elsewhere, so every path comparison goes through this
        // canonicalization instead of raw strings.
        public static string FinalPath(string path)
        {
            using (SafeFileHandle h = CreateFileW(path, 0, FILE_SHARE_ALL, IntPtr.Zero, OPEN_EXISTING,
                FILE_FLAG_BACKUP_SEMANTICS, IntPtr.Zero))
            {
                if (h.IsInvalid)
                {
                    throw new IOException("cannot open " + path + " (error " + Marshal.GetLastWin32Error() + ")");
                }
                StringBuilder sb = new StringBuilder(1024);
                int n = GetFinalPathNameByHandleW(h, sb, sb.Capacity, 0);
                if (n > sb.Capacity)
                {
                    sb.Capacity = n;
                    n = GetFinalPathNameByHandleW(h, sb, sb.Capacity, 0);
                }
                if (n <= 0)
                {
                    throw new IOException("cannot resolve the final path of " + path + " (error " + Marshal.GetLastWin32Error() + ")");
                }
                string s = sb.ToString();
                if (s.StartsWith(@"\\?\UNC\")) { s = @"\\" + s.Substring(8); }
                else if (s.StartsWith(@"\\?\")) { s = s.Substring(4); }
                return s.TrimEnd('\\');
            }
        }

        // FullPath returns the absolute form of a path without touching the
        // filesystem: relative segments resolve against the current
        // directory and separators normalize, but every component keeps the
        // spelling it was given. Path.GetFullPath is not a substitute here -
        // it silently rewrites 8.3 short components (RUNNER~1) to their
        // long names, and paths the installer prints or stores must echo
        // the form the environment or the operator handed in.
        public static string FullPath(string path)
        {
            StringBuilder sb = new StringBuilder(1024);
            int n = GetFullPathNameW(path, sb.Capacity, sb, IntPtr.Zero);
            if (n > sb.Capacity)
            {
                sb.Capacity = n;
                n = GetFullPathNameW(path, sb.Capacity, sb, IntPtr.Zero);
            }
            if (n <= 0)
            {
                throw new IOException("cannot resolve the full path of " + path + " (error " + Marshal.GetLastWin32Error() + ")");
            }
            return sb.ToString();
        }

        // TryCreateDirectory creates the directory and reports false when it
        // already exists - a single system call, so two concurrent callers
        // cannot both succeed.
        public static bool TryCreateDirectory(string path)
        {
            if (CreateDirectoryW(path, IntPtr.Zero)) { return true; }
            int err = Marshal.GetLastWin32Error();
            if (err == ERROR_ALREADY_EXISTS) { return false; }
            throw new IOException("cannot create " + path + " (error " + err + ")");
        }

        // PinnedValidator accepts a server presenting exactly the certificate
        // with the given thumbprint; anything else keeps the default validation.
        public static Func<HttpRequestMessage, X509Certificate2, X509Chain, SslPolicyErrors, bool> PinnedValidator(string thumbprint)
        {
            return delegate(HttpRequestMessage message, X509Certificate2 cert, X509Chain chain, SslPolicyErrors errors)
            {
                if (errors == SslPolicyErrors.None) { return true; }
                return cert != null && string.Equals(cert.Thumbprint, thumbprint, StringComparison.OrdinalIgnoreCase);
            };
        }
    }
}
'@
        if ($PSVersionTable.PSVersion.Major -lt 6) {
            Add-Type -TypeDefinition $source -ReferencedAssemblies @('System.Net.Http') | Out-Null
        } else {
            Add-Type -TypeDefinition $source | Out-Null
        }
    }

    # One HttpClient for every request. Redirects are never followed
    # automatically: a tokened request must not carry its Authorization
    # header to a redirect target, so callers handle redirects themselves.
    function Get-Http {
        if ($S.Http) { return $S.Http }
        if ($PSVersionTable.PSVersion.Major -lt 6) {
            [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        }
        $handler = New-Object System.Net.Http.HttpClientHandler
        $handler.AllowAutoRedirect = $false
        if ($env:RANCHER_CLI_INSTALL_TEST_PIN_CERT) {
            $pinned = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2($env:RANCHER_CLI_INSTALL_TEST_PIN_CERT)
            $handler.ServerCertificateCustomValidationCallback = [RancherCliInstall.Native]::PinnedValidator($pinned.Thumbprint)
        }
        $client = New-Object System.Net.Http.HttpClient($handler)
        $client.Timeout = [TimeSpan]::FromMinutes(5)
        $client.DefaultRequestHeaders.UserAgent.ParseAdd('rancher-cli-install')
        $S.Http = $client
        return $client
    }

    # Invoke-Http <method> <url> [-Auth] [-Accept a] [-OutFile f]: one request,
    # no redirect following. Returns @{ Code; Location; Body } where Body is
    # the response text unless -OutFile received a 200 body. -Auth attaches
    # the token when one is set.
    function Invoke-Http {
        param(
            [string]$Method,
            [string]$Url,
            [switch]$Auth,
            [string]$Accept,
            [string]$OutFile
        )
        $client = Get-Http
        $request = New-Object System.Net.Http.HttpRequestMessage((New-Object System.Net.Http.HttpMethod($Method)), $Url)
        if ($Auth -and $S.Token) {
            $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer', $S.Token)
        }
        if ($Accept) { $request.Headers.Accept.ParseAdd($Accept) }
        try {
            $response = $client.SendAsync($request, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        } catch {
            $inner = $_.Exception
            while ($inner.InnerException) { $inner = $inner.InnerException }
            Fail "request failed: $Method $Url - $($inner.Message)"
        }
        try {
            $code = [int]$response.StatusCode
            $location = $null
            if ($response.Headers.Location) { $location = $response.Headers.Location.ToString() }
            $body = $null
            if ($OutFile -and $code -eq 200) {
                $stream = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
                $file = [IO.File]::Create($OutFile)
                try { $stream.CopyTo($file) } finally { $file.Dispose(); $stream.Dispose() }
            } elseif ($Method -ne 'HEAD') {
                $body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            }
            return @{ Code = $code; Location = $location; Body = $body }
        } finally {
            $response.Dispose()
        }
    }

    # Invoke-Process <exe> <args>: runs a program with captured output and
    # no shell in between. Returns @{ Code; Out; Err }. Arguments are quoted
    # for the command-line convention Go and .NET programs parse.
    function Invoke-Process([string]$Exe, [string[]]$Arguments) {
        $quoted = foreach ($a in $Arguments) {
            if ($a -match '[\s"]' -or $a -eq '') { '"' + ($a -replace '(\\*)"', '$1$1\"' -replace '(\\+)$', '$1$1') + '"' } else { $a }
        }
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $Exe
        $psi.Arguments = ($quoted -join ' ')
        $psi.UseShellExecute = $false
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $psi.RedirectStandardInput = $true
        $psi.CreateNoWindow = $true
        $p = New-Object System.Diagnostics.Process
        $p.StartInfo = $psi
        try {
            [void]$p.Start()
        } catch {
            return @{ Code = 127; Out = ''; Err = $_.Exception.Message }
        }
        $p.StandardInput.Close()
        $stdout = $p.StandardOutput.ReadToEndAsync()
        $stderr = $p.StandardError.ReadToEndAsync()
        $p.WaitForExit()
        return @{ Code = $p.ExitCode; Out = $stdout.GetAwaiter().GetResult(); Err = $stderr.GetAwaiter().GetResult() }
    }

    function Find-Command([string]$Name) {
        $c = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($c) { return $c.Source }
        return $null
    }

    function Test-Tag([string]$Candidate) {
        if ($Candidate -notmatch $TagRe) {
            Fail "tag '$Candidate' is not a valid release tag (vX.Y.Z or vX.Y.Z-{alpha|beta|rc}.N)"
        }
    }

    # Get-TagRank <tag>: MAJ MIN PAT PRERANK PREN for a valid release tag.
    # Prerelease classes order alpha < beta < rc < stable (stable rank 3).
    function Get-TagRank([string]$T) {
        $t = $T.Substring(1)
        $dash = $t.IndexOf('-')
        $core = $t
        $pre = ''
        if ($dash -ge 0) { $core = $t.Substring(0, $dash); $pre = $t.Substring($dash + 1) }
        $parts = $core.Split('.')
        $rank = @([int]$parts[0], [int]$parts[1], [int]$parts[2], 3, 0)
        if ($pre) {
            $dot = $pre.IndexOf('.')
            $rank[3] = @{ alpha = 0; beta = 1; rc = 2 }[$pre.Substring(0, $dot)]
            $rank[4] = [int]$pre.Substring($dot + 1)
        }
        return $rank
    }

    function Test-TagOlderThan([string]$A, [string]$B) {
        $ra = Get-TagRank $A
        $rb = Get-TagRank $B
        for ($i = 0; $i -lt 5; $i++) {
            if ($ra[$i] -lt $rb[$i]) { return $true }
            if ($ra[$i] -gt $rb[$i]) { return $false }
        }
        return $false
    }

    function Resolve-Token {
        if ($S.Token) { return }
        $gh = Find-Command 'gh'
        if (-not $gh) { return }
        $hostName = ([Uri]$S.BaseUrl).Host.Trim('[', ']')
        $r = Invoke-Process $gh @('auth', 'token', '--hostname', $hostName)
        if ($r.Code -ne 0) { return }
        $t = $r.Out.Trim()
        if (-not $t) { return }
        $S.Token = $t
        $S.UsedGhToken = $true
    }

    function Resolve-Platform {
        $raw = $env:PROCESSOR_ARCHITEW6432
        if (-not $raw) { $raw = $env:PROCESSOR_ARCHITECTURE }
        # Accepted architectures must match the windows/* targets of scripts/release-build.
        switch ($raw) {
            'AMD64' { $S.Arch = 'amd64' }
            default { Fail "unsupported platform windows/$raw - download manually: $($S.BaseUrl)/$Repo/releases" }
        }
    }

    function Resolve-Tag {
        if ($Version) {
            Test-Tag $Version
            $S.Tag = $Version
            return
        }
        if ($Pre) {
            Resolve-PreTag
            return
        }
        # Stable latest: one request, the tag comes from the redirect Location.
        # A transport failure is distinct from absence and never reads as
        # "no stable release".
        $r = Invoke-Http 'HEAD' "$($S.BaseUrl)/$Repo/releases/latest" -Auth
        if ($r.Code -eq 404) {
            if (-not $S.Token) {
                Fail "no stable release found - use -Pre or -Version <tag>"
            }
            Fail "no stable release found for this GitHub token - use -Pre or -Version <tag>"
        }
        if ($r.Code -lt 300 -or $r.Code -gt 399) {
            Fail "could not look up the latest stable release (HTTP $($r.Code)) - check the GitHub token and the rate limit, then retry"
        }
        if (-not $r.Location -or $r.Location -notmatch '/tag/([^/]+)$') {
            Fail "could not look up the latest stable release - the response (HTTP $($r.Code)) did not redirect to a release tag URL"
        }
        $S.Tag = $Matches[1]
        Test-Tag $S.Tag
    }

    # -Pre: page the releases list; the newest published (draft:false) entry wins.
    function Resolve-PreTag {
        $page = 1
        while ($true) {
            # Stop after 100 pages: a backstop against non-API responses
            # (captive portal, JSON object); not a release search window.
            if ($page -gt 100) { Fail "release listing: stopped after 100 pages - the response may not come from the GitHub API, or the listing never ends" }
            $r = Invoke-Http 'GET' "$($S.ApiUrl)/repos/$Repo/releases?per_page=100&page=$page" -Auth -Accept 'application/vnd.github+json'
            if ($r.Code -ne 200) {
                if (-not $S.Token) {
                    Fail "release listing request failed (page $page, HTTP $($r.Code)) - a network or server failure, or GitHub limiting requests without a token; wait and retry, or set GH_TOKEN or GITHUB_TOKEN"
                }
                Fail "release listing request failed (page $page, HTTP $($r.Code))"
            }
            # A JSON array must stay one array object here: Windows PowerShell
            # never unrolls it, PowerShell 7 does unless told not to. Then a
            # body of null, like unparsable text, is a $null result, while an
            # exhausted listing is an empty array.
            try {
                if ($PSVersionTable.PSVersion.Major -lt 6) { $parsed = ConvertFrom-Json $r.Body } else { $parsed = ConvertFrom-Json $r.Body -NoEnumerate }
            } catch { $parsed = $null }
            if ($null -eq $parsed) { Fail "release listing parse failed (page $page) - unexpected response (captive portal?)" }
            $list = @($parsed)
            if ($list.Count -eq 0) { Fail "no published release found in the release listing" }
            foreach ($rel in $list) {
                if ($rel.draft) { continue }
                Test-Tag ([string]$rel.tag_name)
                $S.Tag = [string]$rel.tag_name
                $S.ReleaseJson = $rel
                return
            }
            $page++
        }
    }

    # Resolve-AssetIds: with a token, the numeric asset ids the API download
    # endpoint needs. A metadata failure is remembered for the download error
    # and the install falls back to the release download URLs.
    function Resolve-AssetIds {
        if (-not $S.Token) { return }
        $rel = $S.ReleaseJson
        if (-not $rel) {
            $r = Invoke-Http 'GET' "$($S.ApiUrl)/repos/$Repo/releases/tags/$($S.Tag)" -Auth -Accept 'application/vnd.github+json'
            if ($r.Code -ne 200) {
                $S.MetadataFailCode = $r.Code
                Write-Warn "release metadata not readable with this GitHub token (HTTP $($r.Code)) - falling back to the release download URL"
                return
            }
            $rel = ConvertFrom-Json $r.Body
        }
        $ids = @{}
        if ($rel.PSObject.Properties['assets']) {
            foreach ($a in @($rel.assets)) {
                if ($a -and -not $ids.ContainsKey([string]$a.name)) { $ids[[string]$a.name] = [string]$a.id }
            }
        }
        foreach ($name in @($S.CliAsset, $S.HelperAsset, 'SHA256SUMS')) {
            if (-not $ids[$name]) { Fail "asset not found in release $($S.Tag): $name - verify the release assets exist" }
        }
        $S.CliAssetId = $ids[$S.CliAsset]
        $S.HelperAssetId = $ids[$S.HelperAsset]
        $S.SumsAssetId = $ids['SHA256SUMS']
    }

    # Get-ReleaseAsset <id> <out> <name>: downloads through the API asset
    # endpoint. A 200 already wrote the file. A redirect names the signed
    # storage URL, fetched with a second request that carries no token -
    # the token never travels past the API host.
    function Get-ReleaseAsset([string]$Id, [string]$Out, [string]$Name) {
        $r = Invoke-Http 'GET' "$($S.ApiUrl)/repos/$Repo/releases/assets/$Id" -Auth -Accept 'application/octet-stream' -OutFile $Out
        if ($r.Code -eq 200) { return }
        if ($r.Code -ge 300 -and $r.Code -le 399) {
            if (-not $r.Location) { Fail "download failed: $Name (asset id $Id, redirect with no Location)" }
            $cdn = Invoke-Http 'GET' $r.Location -OutFile $Out
            if ($cdn.Code -ne 200) { Fail "download failed: $Name (asset id $Id, the redirect URL answered HTTP $($cdn.Code))" }
            return
        }
        Fail "download failed: $Name (asset id $Id, HTTP $($r.Code))"
    }

    function Get-DirectAsset([string]$Url, [string]$Out) {
        $r = Invoke-Http 'GET' $Url -Auth -OutFile $Out
        if ($r.Code -eq 200) { return $true }
        if ($r.Code -ge 300 -and $r.Code -le 399 -and $r.Location) {
            $r = Invoke-Http 'GET' $r.Location -OutFile $Out
            if ($r.Code -eq 200) { return $true }
        }
        return $false
    }

    # Read-Sums parses the downloaded SHA256SUMS once: asset name (compared
    # exactly, case included) to every sha256 hex the file declares for it.
    function Read-Sums {
        $sums = New-Object System.Collections.Hashtable
        foreach ($line in [IO.File]::ReadAllLines((Join-Path $S.Work 'SHA256SUMS'))) {
            $m = [regex]::Match($line.TrimEnd(), '^([0-9a-fA-F]{64})\s+\*?(\S.*)$')
            if (-not $m.Success) { continue }
            $name = $m.Groups[2].Value
            if (-not $sums.ContainsKey($name)) { $sums[$name] = @() }
            $sums[$name] += $m.Groups[1].Value.ToLowerInvariant()
        }
        $S.Sums = $sums
    }

    # Get-ExpectedSum <asset>: every sha256 hex SHA256SUMS declares for the
    # asset - one element when the file is well formed.
    function Get-ExpectedSum([string]$Asset) {
        if ($S.Sums.ContainsKey($Asset)) { return , [string[]]$S.Sums[$Asset] }
        return , [string[]]@()
    }

    function Get-FileSum([string]$Path) {
        $sha = [System.Security.Cryptography.SHA256]::Create()
        try {
            $stream = [IO.File]::OpenRead($Path)
            try { $hash = $sha.ComputeHash($stream) } finally { $stream.Dispose() }
        } finally {
            $sha.Dispose()
        }
        return ([BitConverter]::ToString($hash) -replace '-', '').ToLowerInvariant()
    }

    function Test-Checksums {
        # Each asset independently: exactly one entry, then its digest.
        foreach ($a in @($S.CliAsset, $S.HelperAsset)) {
            $expected = Get-ExpectedSum $a
            if ($expected.Count -ne 1) { Fail "SHA256SUMS must contain exactly one entry for $a (found $($expected.Count))" }
            if ((Get-FileSum (Join-Path $S.Work $a)) -ne $expected[0]) { Fail "checksum verification failed for $a" }
        }
        Write-Note "checksums verified against SHA256SUMS: $($S.CliAsset), $($S.HelperAsset)"
    }

    # Attestation: capability probe, then verify. Anything short of a gh that
    # is installed, supports --signer-workflow and is logged in takes the
    # unavailable path (notice; fatal only with -VerifyAttestation). A capable
    # client's verification failure is fatal.
    function Test-GhCapable {
        $gh = Find-Command 'gh'
        if (-not $gh) { return $null }
        $r = Invoke-Process $gh @('attestation', 'verify', '--help')
        if ($r.Out -notmatch 'signer-workflow') { return $null }
        $r = Invoke-Process $gh @('auth', 'status')
        if ($r.Code -ne 0) { return $null }
        return $gh
    }

    # A same-tag version dir is reused only when both binaries match the
    # freshly downloaded SHA256SUMS.
    function Test-VersionDirIntact([string]$Dir) {
        foreach ($pair in @(@($CliName, $S.CliAsset), @($HelperName, $S.HelperAsset))) {
            $p = Join-Path $Dir $pair[0]
            if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { return $false }
            $expected = Get-ExpectedSum $pair[1]
            if ($expected.Count -ne 1 -or (Get-FileSum $p) -ne $expected[0]) { return $false }
        }
        return $true
    }

    function Test-ReparsePoint([string]$Path) {
        $item = Get-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
        return ($null -ne $item) -and (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
    }

    function Test-Junction([string]$Path) {
        $item = Get-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
        return ($null -ne $item) -and (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) -and ($item.LinkType -eq 'Junction')
    }

    function Get-JunctionTarget([string]$Path) {
        $t = (Get-Item -LiteralPath $Path -Force).Target
        if ($t -is [array]) { $t = $t[0] }
        return (Get-FullPath ([string]$t)).TrimEnd('\')
    }

    # Get-FinalPath canonicalizes an existing path to the filesystem's own
    # final long-form name, resolving through junctions - the only compare
    # form safe against 8.3 short names in the environment. Comparisons
    # canonicalize both sides at the compare site; state and output keep
    # the path forms they were handed.
    function Get-FinalPath([string]$Path) {
        return [RancherCliInstall.Native]::FinalPath($Path)
    }

    # Get-FullPath makes a path absolute without rewriting its spelling -
    # unlike [IO.Path]::GetFullPath, which expands 8.3 short components to
    # their long names. Paths that reach output or the receipt go through
    # this, so they echo the form the environment or the operator gave.
    function Get-FullPath([string]$Path) {
        return [RancherCliInstall.Native]::FullPath($Path)
    }

    # Resolve-Reparse follows a file or directory through reparse points -
    # junctions and symbolic links, a relative target taken against the
    # link's own directory - and returns where the chain ends. The hop cap
    # breaks cycles: a path still a reparse point at the cap is returned as
    # it stands.
    function Resolve-Reparse([string]$Path) {
        $p = [IO.Path]::GetFullPath($Path)
        for ($i = 0; $i -lt 8; $i++) {
            $item = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
            if ($null -eq $item -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) { break }
            $t = $item.Target
            if ($t -is [array]) { $t = $t[0] }
            if (-not $t) { break }
            $t = [string]$t
            if (-not [IO.Path]::IsPathRooted($t)) { $t = Join-Path (Split-Path -Parent $p) $t }
            $p = [IO.Path]::GetFullPath($t)
        }
        return $p.TrimEnd('\')
    }

    # Remove-Tree removes a directory. A reparse point is unlinked, never
    # followed: Remove-Item -Recurse on a junction could delete its target's
    # contents.
    function Remove-Tree([string]$Path) {
        if (Test-ReparsePoint $Path) { [IO.Directory]::Delete($Path); return }
        Remove-Item -LiteralPath $Path -Recurse -Force
    }

    # Wait-Hold: test-only seam - blocks while the named file exists.
    function Wait-Hold([string]$File) {
        if (-not $File) { return }
        while (Test-Path -LiteralPath $File) { Start-Sleep -Milliseconds 100 }
    }

    function Test-OnPath([string]$Dir) {
        $want = $Dir.TrimEnd('\')
        foreach ($d in ($env:Path -split ';')) {
            if ($d -and $d.TrimEnd('\') -ieq $want) { return $true }
        }
        return $false
    }

    # Receipt: atomic replace, best-effort (warn, never fail the install).
    function Write-Receipt {
        try {
            [IO.Directory]::CreateDirectory($S.ConfigDir) | Out-Null
            $receipt = [ordered]@{
                version     = $S.Tag.Substring(1)
                tag         = $S.Tag
                binDir      = $S.BinDir
                dataDir     = $S.DataDir
                layout      = 'versioned-dir'
                channel     = 'script'
                installedAt = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
            }
            $tmp = Join-Path $S.ConfigDir ".receipt.$PID"
            $dest = Join-Path $S.ConfigDir 'install-receipt.json'
            [IO.File]::WriteAllText($tmp, (ConvertTo-Json $receipt -Compress) + "`n")
            if (Test-Path -LiteralPath $dest) { [IO.File]::Replace($tmp, $dest, [NullString]::Value) } else { [IO.File]::Move($tmp, $dest) }
        } catch {
            Write-Warn "install receipt not written: $($_.Exception.Message)"
        }
    }

    function Initialize-Args {
        if ($Version -and $Pre) { Fail "-Version and -Pre are mutually exclusive" }
        foreach ($u in @($S.BaseUrl, $S.ApiUrl)) {
            if (-not $u.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) { Fail "install base URL must use https: $u" }
        }
        if (-not $env:LOCALAPPDATA) { Fail "LOCALAPPDATA is not set" }
        if (-not $env:APPDATA) { Fail "APPDATA is not set" }
        $S.DataDir = Join-Path $env:LOCALAPPDATA 'rancher-cli'
        $S.ConfigDir = Join-Path $env:APPDATA 'rancher-cli'
        $S.VersionsDir = Join-Path $S.DataDir 'versions'
        if (-not $S.BinDir) { $S.BinDir = Join-Path $S.DataDir 'bin' }
        $S.BinDir = (Get-FullPath $S.BinDir).TrimEnd('\')
        if ([IO.Path]::GetPathRoot($S.BinDir).TrimEnd('\') -ieq $S.BinDir) { Fail "the bin directory cannot be a drive root: $($S.BinDir)" }
    }

    function Resolve-Release {
        Resolve-Tag
        # Asset names must match the build loop in scripts/release-build - the two lists stay in lockstep.
        $S.CliAsset = "rancher-$Os-$($S.Arch).exe"
        $S.HelperAsset = "rancher-credential-vault-$Os-$($S.Arch).exe"
        Resolve-AssetIds
        if (-not $DryRun) { return $true }
        # Dry run: resolution requests only - no filesystem writes, no lock,
        # no attestation.
        $o = [Console]::Out
        $o.WriteLine("tag: $($S.Tag)")
        $o.WriteLine("platform: $Os/$($S.Arch)")
        if ($S.CliAssetId) {
            $o.WriteLine("asset: $($S.ApiUrl)/repos/$Repo/releases/assets/$($S.CliAssetId)")
            $o.WriteLine("asset: $($S.ApiUrl)/repos/$Repo/releases/assets/$($S.HelperAssetId)")
            $o.WriteLine("asset: $($S.ApiUrl)/repos/$Repo/releases/assets/$($S.SumsAssetId)")
        } else {
            $o.WriteLine("asset: $($S.BaseUrl)/$Repo/releases/download/$($S.Tag)/$($S.CliAsset)")
            $o.WriteLine("asset: $($S.BaseUrl)/$Repo/releases/download/$($S.Tag)/$($S.HelperAsset)")
            $o.WriteLine("asset: $($S.BaseUrl)/$Repo/releases/download/$($S.Tag)/SHA256SUMS")
        }
        $o.WriteLine("bin-dir: $($S.BinDir)")
        $o.WriteLine("data-dir: $($S.DataDir)")
        return $false
    }

    function Get-Release {
        $S.Work = Join-Path ([IO.Path]::GetTempPath()) ("rancher-cli-install.$PID." + [IO.Path]::GetRandomFileName())
        [IO.Directory]::CreateDirectory($S.Work) | Out-Null
        Write-Note "downloading $($S.Tag) for $Os/$($S.Arch)"
        if ($S.CliAssetId) {
            # With a token, download through the GitHub API asset endpoint;
            # without asset ids the plain release download URLs are used.
            Get-ReleaseAsset $S.CliAssetId (Join-Path $S.Work $S.CliAsset) $S.CliAsset
            Get-ReleaseAsset $S.HelperAssetId (Join-Path $S.Work $S.HelperAsset) $S.HelperAsset
            Get-ReleaseAsset $S.SumsAssetId (Join-Path $S.Work 'SHA256SUMS') 'SHA256SUMS'
            return
        }
        $dl = "$($S.BaseUrl)/$Repo/releases/download/$($S.Tag)"
        if (-not (Get-DirectAsset "$dl/$($S.CliAsset)" (Join-Path $S.Work $S.CliAsset))) {
            $msg = "download failed: $dl/$($S.CliAsset) (404 = tag '$($S.Tag)' has no such asset)"
            if ($S.Token -and $S.MetadataFailCode) {
                $msg += " - or the GitHub token is expired or lacks access; release metadata was also unreadable (HTTP $($S.MetadataFailCode))"
            }
            Fail $msg
        }
        if (-not (Get-DirectAsset "$dl/$($S.HelperAsset)" (Join-Path $S.Work $S.HelperAsset))) { Fail "download failed: $dl/$($S.HelperAsset)" }
        if (-not (Get-DirectAsset "$dl/SHA256SUMS" (Join-Path $S.Work 'SHA256SUMS'))) { Fail "download failed: $dl/SHA256SUMS" }
    }

    function Test-Release {
        Read-Sums
        Test-Checksums
        $gh = Test-GhCapable
        if ($gh) {
            foreach ($b in @((Join-Path $S.Work $S.CliAsset), (Join-Path $S.Work $S.HelperAsset))) {
                $r = Invoke-Process $gh @('attestation', 'verify', $b, '--repo', $Repo, '--signer-workflow', "$Repo/.github/workflows/release.yaml")
                if ($r.Code -ne 0) { Fail "build provenance attestation failed for $b" }
            }
            Write-Note "build provenance attestation verified"
        } elseif ($VerifyAttestation) {
            Fail "-VerifyAttestation needs a gh CLI whose 'gh attestation verify' supports --signer-workflow, and a gh login (gh auth login)"
        } else {
            Write-Note "notice: attestation skipped (gh is not installed, its 'gh attestation verify' does not support --signer-workflow, or gh is not logged in)"
        }
    }

    # The bin directory is the installer's Windows directory link (NTFS
    # junction) to the current version. Before any layout mutation it may be
    # absent, a Windows directory link (ours or foreign), an empty directory,
    # or a directory holding only a prior by-hand rancher.exe/
    # rancher-credential-vault.exe pair; anything else is refused.
    function Confirm-BinDir {
        Restore-BinDir
        if (-not (Test-Path -LiteralPath $S.BinDir)) { return }
        $item = Get-Item -LiteralPath $S.BinDir -Force
        if (-not $item.PSIsContainer) { Fail "$($S.BinDir) is a file - refusing to replace it" }
        if (Test-Junction $S.BinDir) {
            # Ownership is decided in final form on both sides: the junction
            # may have recorded a spelling of the versions dir (an 8.3 short
            # form) that never string-matches the one this run was handed.
            # The notice echoes the stored target as recorded.
            $resolved = Get-FinalPath $S.BinDir
            if ($resolved.StartsWith("$(Get-FinalPath $S.VersionsDir)\", [StringComparison]::OrdinalIgnoreCase)) {
                $S.CurrentTarget = $resolved
            } else {
                Write-Note "notice: replacing existing Windows directory link (NTFS junction) $($S.BinDir) -> $(Get-JunctionTarget $S.BinDir)"
            }
            return
        }
        if (Test-ReparsePoint $S.BinDir) { Fail "$($S.BinDir) is a symbolic link - refusing to replace it" }
        foreach ($e in @(Get-ChildItem -LiteralPath $S.BinDir -Force)) {
            if ($e.PSIsContainer -or ($e.Name -ine $CliName -and $e.Name -ine $HelperName)) {
                Fail "$($S.BinDir) holds other files - the installer makes the bin directory a Windows directory link (NTFS junction) to the current version; pick an empty or new directory with -BinDir"
            }
        }
    }

    # A plain bin dir (a prior by-hand pair or an empty directory) moves aside
    # to <bin>.old.<pid> beside itself before the junction takes its place,
    # and is removed only once the junction is verified. Get-BinDirAsides
    # lists every aside beside the bin dir, oldest run first. Callers re-root
    # an entry's Name on the bin dir's parent as given: the provider expands
    # 8.3 short components in FullName, and paths that reach output must
    # echo the environment's own spelling.
    function Get-BinDirAsides {
        $parent = Split-Path -Parent $S.BinDir
        $leaf = Split-Path -Leaf $S.BinDir
        return @(Get-ChildItem -LiteralPath $parent -Force -Directory -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like "$leaf.old.*" -and ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0 } |
            Sort-Object LastWriteTimeUtc)
    }

    # Restore-BinDir puts back a directory an interrupted run moved aside and
    # never replaced: with the bin dir absent, the aside holds the only copy
    # of the pair, so it becomes the bin dir again and the run proceeds as
    # over any by-hand pair. Further asides are collected after the switch.
    function Restore-BinDir {
        if (Test-Path -LiteralPath $S.BinDir) { return }
        $asides = Get-BinDirAsides
        if ($asides.Count -eq 0) { return }
        $aside = Join-Path (Split-Path -Parent $S.BinDir) $asides[$asides.Count - 1].Name
        Write-Note "notice: restoring $aside to $($S.BinDir) - an earlier install was stopped"
        [IO.Directory]::Move($aside, $S.BinDir)
    }

    # Move-BinDirAside takes a plain bin dir out of the junction's way in one
    # rename. A running binary does not block the rename of its directory;
    # it only blocks the aside's later removal, which the next run retries.
    # A stale aside of the same name (a recycled pid) that cannot be removed
    # is left for the post-switch GC and the name is made unique with a
    # counter, so the rename never targets an existing directory.
    function Move-BinDirAside {
        foreach ($e in @(Get-ChildItem -LiteralPath $S.BinDir -Force)) {
            Write-Note "notice: replacing existing file $(Join-Path $S.BinDir $e.Name)"
        }
        $aside = "$($S.BinDir).old.$PID"
        if (Test-Path -LiteralPath $aside) { try { Remove-Tree $aside } catch { } }
        $n = 0
        while (Test-Path -LiteralPath $aside) {
            $n++
            $aside = "$($S.BinDir).old.$PID.$n"
        }
        $S.Aside = $aside
        [IO.Directory]::Move($S.BinDir, $S.Aside)
    }

    function Add-Stage {
        [IO.Directory]::CreateDirectory($S.VersionsDir) | Out-Null
        [IO.Directory]::CreateDirectory((Split-Path -Parent $S.BinDir)) | Out-Null

        $lockPath = Join-Path $S.DataDir '.install.lock'
        if (-not [RancherCliInstall.Native]::TryCreateDirectory($lockPath)) {
            Fail "another install is in progress (lock: $lockPath); if none is, removing the lock directory is safe"
        }
        $S.Lock = $lockPath

        Confirm-BinDir

        $S.VDir = Join-Path $S.VersionsDir $S.Tag
        $intact = (Test-Path -LiteralPath $S.VDir) -and (Test-VersionDirIntact $S.VDir)

        # GC under the lock: staging litter and version dirs the bin junction
        # does not reference. An intact same-tag dir - staged by a run that
        # never finished its switch - is kept and reused below. A directory a
        # running binary still holds is left for the next run.
        foreach ($d in @(Get-ChildItem -LiteralPath $S.VersionsDir -Force)) {
            # Each entry is re-rooted on the versions dir as given: the
            # provider expands 8.3 short components in FullName, which would
            # neither string-match VDir's spelling nor print in the
            # operator's own form. CurrentTarget holds a final-form path, so
            # that compare resolves the entry to final form too.
            $full = Join-Path $S.VersionsDir $d.Name
            if ($S.CurrentTarget -and (Get-FinalPath $full) -ieq $S.CurrentTarget) { continue }
            if ($intact -and $full -ieq $S.VDir) { continue }
            try { Remove-Tree $full } catch { Write-Warn "cannot remove ${full}: $($_.Exception.Message)" }
        }

        if ($intact) {
            Write-Note "version $($S.Tag) already present and intact - making the bin directory a Windows directory link (NTFS junction) to it"
            return
        }
        if (Test-Path -LiteralPath $S.VDir) { $S.VDir = Join-Path $S.VersionsDir "$($S.Tag).2" }
        $S.StagingDir = Join-Path $S.VersionsDir "$($S.Tag).staging.$PID"
        [IO.Directory]::CreateDirectory($S.StagingDir) | Out-Null
        [IO.File]::Copy((Join-Path $S.Work $S.CliAsset), (Join-Path $S.StagingDir $CliName), $true)
        [IO.File]::Copy((Join-Path $S.Work $S.HelperAsset), (Join-Path $S.StagingDir $HelperName), $true)
        Wait-Hold $env:RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING
        if (Test-Path -LiteralPath $S.VDir) { Remove-Tree $S.VDir }
        [IO.Directory]::Move($S.StagingDir, $S.VDir)
        $S.StagingDir = $null
    }

    function Switch-Current {
        # Name the version change before switching: an upgrade is a note, a
        # downgrade a warning - rollback is documented, never silent.
        if ($S.CurrentTarget) {
            $prev = Split-Path -Leaf $S.CurrentTarget
            if ($prev -ne $S.Tag) {
                if ($prev -match $TagRe) {
                    if (Test-TagOlderThan $S.Tag $prev) { Write-Warn "downgrading from $prev to $($S.Tag)" } else { Write-Note "upgrading from $prev to $($S.Tag)" }
                } else {
                    Write-Note "replacing $prev with $($S.Tag)"
                }
            }
        }

        # The switch. The new junction is created beside the bin dir first, so
        # the new pair is complete on disk before anything existing moves.
        # Over an absent bin dir or a junction the rename is one operation:
        # a kill before it leaves the old pair, a kill after it the new pair.
        # A rename cannot replace a plain directory, so a by-hand bin dir
        # moves aside in one rename first and the junction is renamed in
        # second: a kill between the two renames leaves the bin dir absent
        # for that instant, with the old pair intact in the aside and the
        # next run restoring it before it switches. The aside is removed
        # last, once the junction is verified.
        #
        # Cleanup contract: the driver's finally removes the staging junction,
        # the staging dir, the lock, and the work dir on any error and on a
        # Ctrl-C that lands in script code, and restores the aside when the
        # bin dir is absent. A kill, or a Ctrl-C arriving inside a native
        # call or the HTTP wait, can end the process before the finally
        # runs. The next run's GC collects staging litter and asides once
        # its own switch is verified, and its aside recovery restores a
        # missing bin dir; only a leftover lock needs the user's hand, and
        # its error message says so.
        $S.StagingLink = Join-Path (Split-Path -Parent $S.BinDir) (".$(Split-Path -Leaf $S.BinDir).$PID")
        if (Test-Path -LiteralPath $S.StagingLink) { Remove-Tree $S.StagingLink }
        New-Item -ItemType Junction -Path $S.StagingLink -Target $S.VDir | Out-Null
        if ((Test-Path -LiteralPath $S.BinDir) -and -not (Test-Junction $S.BinDir)) {
            Move-BinDirAside
            Wait-Hold $env:RANCHER_CLI_INSTALL_HOLD_AFTER_ASIDE
        }
        [RancherCliInstall.Native]::ReplaceDirectory($S.StagingLink, $S.BinDir)
        $S.StagingLink = $null
        if (-not (Test-Junction $S.BinDir) -or (Get-FinalPath $S.BinDir) -ine (Get-FinalPath $S.VDir)) {
            Fail "the bin directory is not a Windows directory link (NTFS junction) to the installed version - rerun the installer"
        }
        Wait-Hold $env:RANCHER_CLI_INSTALL_HOLD_AFTER_SWITCH

        # Post-switch GC: the pre-staging pass kept the then-current version;
        # collect it now unless a running binary still holds it. Beside the
        # bin dir, every aside and every staging junction from an earlier
        # run is litter now that the junction is verified.
        foreach ($d in @(Get-ChildItem -LiteralPath $S.VersionsDir -Force)) {
            $full = Join-Path $S.VersionsDir $d.Name
            if ($full -ieq $S.VDir) { continue }
            try { Remove-Tree $full } catch { Write-Warn "cannot remove old version directory $full (a program from it may still be running) - the next install will retry" }
        }
        foreach ($d in (Get-BinDirAsides)) {
            $full = Join-Path (Split-Path -Parent $S.BinDir) $d.Name
            try { Remove-Tree $full } catch { Write-Warn "cannot remove the moved-aside bin directory $full (a program from it may still be running) - the next install will retry" }
        }
        $S.Aside = $null
        $stale = ".$(Split-Path -Leaf $S.BinDir).*"
        foreach ($d in @(Get-ChildItem -LiteralPath (Split-Path -Parent $S.BinDir) -Force -Directory | Where-Object { $_.Name -like $stale })) {
            if (Test-Junction $d.FullName) { try { Remove-Tree $d.FullName } catch { } }
        }
    }

    function Write-Epilogue {
        foreach ($n in @($CliName, $HelperName)) {
            if (-not (Test-Path -LiteralPath (Join-Path $S.BinDir $n) -PathType Leaf)) { Fail "an installed binary is missing from the bin directory - rerun the installer" }
        }
        $r = Invoke-Process (Join-Path $S.BinDir $CliName) @('--version')
        if ($r.Code -ne 0) { Fail "installed rancher.exe failed to run: $($r.Err.Trim())" }
        Write-Note "installed: $($r.Out.Trim())"
        if (-not (Test-OnPath $S.BinDir)) {
            Write-Note "add to PATH (this session): `$env:Path += ';$($S.BinDir)'"
            Write-Note "add to PATH (every session): [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';$($S.BinDir)', 'User')"
        }
        # Shadow scan: another rancher or rancher-credential-vault elsewhere
        # on PATH can shadow this install by PATH order. Warn, not note, so
        # -Quiet cannot hide a shadowed install. A junction or symbolic link
        # that leads back to the installed binary runs the identical file -
        # not a shadow, so the candidate and its directory are followed
        # through reparse points before the comparison.
        # The comparison runs in the filesystem's final form on both sides:
        # Resolve-Reparse supplies the chain semantics, Get-FinalPath then
        # settles the spelling, so a link back at the install can never
        # read as foreign over an 8.3 short component. Both dirs exist by
        # now - the binaries just ran out of them.
        $binFull = Get-FinalPath $S.BinDir
        $vdirFull = Get-FinalPath $S.VDir
        foreach ($base in @('rancher', 'rancher-credential-vault')) {
            foreach ($c in @(Get-Command $base -All -CommandType Application -ErrorAction SilentlyContinue)) {
                $dir = Resolve-Reparse (Split-Path -Parent (Resolve-Reparse $c.Source))
                # A candidate whose final form cannot be resolved (a PATH
                # entry gone mid-scan) keeps its walked spelling: the scan
                # warns at worst, it never fails the install.
                try { $dir = Get-FinalPath $dir } catch { }
                if ($dir -ieq $binFull -or $dir -ieq $vdirFull) { continue }
                Write-Warn "another $base exists at $($c.Source) - it may shadow this install (check PATH order)"
            }
        }
        Write-Note "next steps:"
        Write-Note "  rancher completion pwsh | Out-String | Invoke-Expression   # shell completion"
        Write-Note "  rancher skill install                                      # skill for coding-agent harnesses"
        Write-Note "  rancher auth login <name> --url <url>                      # log in to a Rancher server"
    }

    try {
        try {
            # Native helpers load first: argument validation resolves the
            # bin dir path through them.
            if ($PSVersionTable.PSVersion.Major -lt 6) { Add-Type -AssemblyName System.Net.Http | Out-Null }
            Import-Native
            Initialize-Args
            Resolve-Token
            if ($S.UsedGhToken) { Write-Note "using gh auth token" }
            Resolve-Platform
            if (-not (Resolve-Release)) { return }
            Get-Release
            Test-Release
            Add-Stage
            Switch-Current
            Write-Receipt
            Write-Epilogue
        } finally {
            if ($S.StagingLink -and (Test-Path -LiteralPath $S.StagingLink)) { try { Remove-Tree $S.StagingLink } catch { } }
            if ($S.Aside -and (Test-Path -LiteralPath $S.Aside) -and -not (Test-Path -LiteralPath $S.BinDir)) { try { [IO.Directory]::Move($S.Aside, $S.BinDir) } catch { } }
            if ($S.StagingDir -and (Test-Path -LiteralPath $S.StagingDir)) { try { Remove-Tree $S.StagingDir } catch { } }
            if ($S.Lock) { try { [IO.Directory]::Delete($S.Lock) } catch { } }
            if ($S.Work -and (Test-Path -LiteralPath $S.Work)) { try { Remove-Item -LiteralPath $S.Work -Recurse -Force } catch { } }
            if ($S.Http) { $S.Http.Dispose() }
        }
    } catch {
        [Console]::Error.WriteLine("error: $($_.Exception.Message)")
        # From a script file the process exit code carries the failure. Under
        # Invoke-Expression in an interactive session exit would close the
        # session, so the failure surfaces as a terminating error instead.
        if ($PSCommandPath) { exit 1 }
        throw "install failed"
    }
}

Install-RancherCli -Version $Version -Pre:$Pre -BinDir $BinDir -VerifyAttestation:$VerifyAttestation -DryRun:$DryRun -Quiet:$Quiet
