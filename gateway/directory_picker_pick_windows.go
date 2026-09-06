//go:build windows

// The Windows native directory chooser: the modern IFileOpenDialog COM
// dialog (FOS_PICKFOLDERS) — the same common dialog the official launcher
// drives — compiled inline through PowerShell's Add-Type C# compiler and
// shown on the host's interactive desktop. A legacy FolderBrowserDialog is
// the fallback when the interop cannot compile (stripped-down .NET). The
// script travels as -EncodedCommand (base64 UTF-16LE) so the console
// codepage cannot mangle the non-ASCII title; the console window itself is
// hidden (only the dialog shows).
package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// pickDialogCSharp is the IFileOpenDialog interop: vtable-order COM
// declarations through GetResult, cancellation as the canonical
// HRESULT_FROM_WIN32(ERROR_CANCELLED).
const pickDialogCSharp = `
using System;
using System.Runtime.InteropServices;

namespace Dsh {
public static class FolderPicker {
    [ComImport, ClassInterface(ClassInterfaceType.None), Guid("DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7")]
    private class FileOpenDialogRCW { }

    [Flags]
    private enum FOS : uint {
        FOS_PICKFOLDERS = 0x20,
        FOS_FORCEFILESYSTEM = 0x40,
        FOS_PATHMUSTEXIST = 0x800,
        FOS_NOREADONLYRETURN = 0x8000
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode, Pack = 4)]
    private struct COMDLG_FILTERSPEC {
        [MarshalAs(UnmanagedType.LPWStr)] public string pszName;
        [MarshalAs(UnmanagedType.LPWStr)] public string pszSpec;
    }

    [ComImport, Guid("D57C7288-D4AD-4768-BE02-9D969532D960"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    private interface IFileOpenDialog {
        [PreserveSig] int Show(IntPtr owner);
        [PreserveSig] int SetFileTypes(uint count, COMDLG_FILTERSPEC[] filters);
        [PreserveSig] int SetFileTypeIndex(uint index);
        [PreserveSig] int GetFileTypeIndex(out uint index);
        [PreserveSig] int Advise(IntPtr pfde, out uint cookie);
        [PreserveSig] int Unadvise(uint cookie);
        [PreserveSig] int SetOptions(FOS fos);
        [PreserveSig] int GetOptions(out FOS fos);
        [PreserveSig] int Open([MarshalAs(UnmanagedType.Interface)] out IntPtr item);
        [PreserveSig] int SetFileName([MarshalAs(UnmanagedType.LPWStr)] string name);
        [PreserveSig] int GetFileName([MarshalAs(UnmanagedType.LPWStr)] out string name);
        [PreserveSig] int SetTitle([MarshalAs(UnmanagedType.LPWStr)] string title);
        [PreserveSig] int SetOkButtonLabel([MarshalAs(UnmanagedType.LPWStr)] string text);
        [PreserveSig] int SetFileNameLabel([MarshalAs(UnmanagedType.LPWStr)] string label);
        [PreserveSig] int GetResult([MarshalAs(UnmanagedType.Interface)] out IShellItem item);
    }

    [ComImport, Guid("43826D1E-E718-42EE-BC55-A1E261C37BFE"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    private interface IShellItem {
        [PreserveSig] int BindToHandler(IntPtr pbc, ref Guid bhid, ref Guid riid, out IntPtr ppv);
        [PreserveSig] int GetParent(out IntPtr ppsi);
        [PreserveSig] int GetDisplayName(uint sigdnName, [MarshalAs(UnmanagedType.LPWStr)] out string name);
    }

    [DllImport("ole32.dll")]
    private static extern int CoInitializeEx(IntPtr reserved, uint coInit);

    [DllImport("user32.dll")]
    private static extern IntPtr GetDesktopWindow();

    public static string Pick(string title) {
        // APARTMENTTHREADED | DISABLE_OLE1DDE; an already-STA host thread
        // answers RPC_E_CHANGED_MODE here, which is benign.
        CoInitializeEx(IntPtr.Zero, 0x6);
        IFileOpenDialog dialog = (IFileOpenDialog)new FileOpenDialogRCW();
        try {
            dialog.SetOptions(FOS.FOS_PICKFOLDERS | FOS.FOS_FORCEFILESYSTEM | FOS.FOS_PATHMUSTEXIST | FOS.FOS_NOREADONLYRETURN);
            dialog.SetTitle(title);
            int hr = dialog.Show(GetDesktopWindow());
            if (hr == unchecked((int)0x800704C7)) return null; // ERROR_CANCELLED
            if (hr != 0) Marshal.ThrowExceptionForHR(hr);
            IShellItem item;
            dialog.GetResult(out item);
            string path;
            item.GetDisplayName(0x80058000 /* SIGDN_FILESYSPATH */, out path);
            return path;
        } finally {
            Marshal.ReleaseComObject(dialog);
        }
    }
}
}
`

// pickDialogScript drives the modern chooser with the legacy dialog as the
// compiled-interop fallback; empty output = the operator cancelled.
const pickDialogScript = "$title = '选择工作区目录'\n" +
	"$csharp = @'\n" + pickDialogCSharp + "'@\n" +
	"$selected = $null\n" +
	"try {\n" +
	"  Add-Type -TypeDefinition $csharp -Language CSharp\n" +
	"  $selected = [Dsh.FolderPicker]::Pick($title)\n" +
	"} catch {\n" +
	"  Add-Type -AssemblyName System.Windows.Forms | Out-Null\n" +
	"  $owner = New-Object System.Windows.Forms.Form -Property @{TopMost=$true;ShowInTaskbar=$false}\n" +
	"  $dlg = New-Object System.Windows.Forms.FolderBrowserDialog\n" +
	"  $dlg.ShowNewFolderButton = $true\n" +
	"  if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { $selected = $dlg.SelectedPath }\n" +
	"}\n" +
	"if ($selected) { [Console]::Out.Write($selected) }\n"

// pickNativeDirectory opens the modern Windows folder chooser and returns
// the selected absolute path. An empty path with a nil error means the
// operator cancelled the dialog (the official pick resolves null on
// cancel — never an error).
func pickNativeDirectory(ctx context.Context) (string, error) {
	encoded := base64.StdEncoding.EncodeToString(utf16LittleEndian(pickDialogScript))
	cmd := exec.CommandContext(ctx, "powershell",
		"-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encoded)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.Output()
	if err != nil {
		// The request context cancelling (page closed) kills the dialog
		// process; surface that as the cancel outcome, not a failure.
		if ctx.Err() != nil {
			return "", nil
		}
		return "", fmt.Errorf("native directory picker failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// utf16LittleEndian encodes s for PowerShell -EncodedCommand.
func utf16LittleEndian(s string) []byte {
	units := utf16.Encode([]rune(s))
	bytes := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		bytes = append(bytes, byte(unit), byte(unit>>8))
	}
	return bytes
}
