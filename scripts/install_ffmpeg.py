import os
import shutil
import tarfile
import zipfile
import urllib.request
import ssl
import platform
import subprocess

# Ignore SSL certificate errors
ssl._create_default_https_context = ssl._create_unverified_context

# Constants
THIRD_PARTY_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "3rd")
FFMPEG_DIR = os.path.join(THIRD_PARTY_DIR, "ffmpeg6")
TEMP_DIR = os.path.join(THIRD_PARTY_DIR, "temp_ffmpeg_download")

# Versions and URLs
# Using BtbN for consistency on Linux/Windows
LINUX_AMD64_URL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n6.1-latest-linux64-gpl-shared-6.1.tar.xz"
LINUX_ARM64_URL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n6.1-latest-linuxarm64-gpl-shared-6.1.tar.xz"
WINDOWS_AMD64_URL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n6.1-latest-win64-gpl-shared-6.1.zip"

def log(msg):
    print(f"[FFmpeg Setup] {msg}")

def download_file(url, dest_path):
    log(f"Downloading {url}...")
    try:
        urllib.request.urlretrieve(url, dest_path)
        return True
    except Exception as e:
        log(f"Error downloading {url}: {e}")
        return False

def extract_archive(path, extract_to):
    log(f"Extracting {path}...")
    try:
        if path.endswith('.tar.xz'):
            with tarfile.open(path, "r:xz") as tar:
                tar.extractall(path=extract_to)
        elif path.endswith('.zip'):
            with zipfile.ZipFile(path, 'r') as zip_ref:
                zip_ref.extractall(extract_to)
        return True
    except Exception as e:
        log(f"Error extracting {path}: {e}")
        return False

def setup_linux():
    log("Setting up Linux libraries...")
    linux_dir = os.path.join(FFMPEG_DIR, "linux")
    if os.path.exists(linux_dir):
        shutil.rmtree(linux_dir)
    
    # Create structure
    os.makedirs(os.path.join(linux_dir, "lib", "amd64"))
    os.makedirs(os.path.join(linux_dir, "lib", "arm64"))
    os.makedirs(os.path.join(linux_dir, "include"))

    # Process AMD64
    f_amd64 = os.path.join(TEMP_DIR, "linux_amd64.tar.xz")
    if download_file(LINUX_AMD64_URL, f_amd64) and extract_archive(f_amd64, TEMP_DIR):
        extracted_root = [d for d in os.listdir(TEMP_DIR) if d.startswith("ffmpeg-n") and "linux64" in d][0]
        src = os.path.join(TEMP_DIR, extracted_root)
        
        # Copy includes (use AMD64 version as the canonical include for Linux)
        shutil.copytree(os.path.join(src, "include"), os.path.join(linux_dir, "include"), dirs_exist_ok=True)
        
        # Copy libs
        src_lib = os.path.join(src, "lib")
        dst_lib = os.path.join(linux_dir, "lib", "amd64")
        shutil.copytree(src_lib, dst_lib, dirs_exist_ok=True)

    # Process ARM64
    f_arm64 = os.path.join(TEMP_DIR, "linux_arm64.tar.xz")
    if download_file(LINUX_ARM64_URL, f_arm64) and extract_archive(f_arm64, TEMP_DIR):
        extracted_root = [d for d in os.listdir(TEMP_DIR) if d.startswith("ffmpeg-n") and "linuxarm64" in d][0]
        src = os.path.join(TEMP_DIR, extracted_root)
        
        # Copy libs
        src_lib = os.path.join(src, "lib")
        dst_lib = os.path.join(linux_dir, "lib", "arm64")
        shutil.copytree(src_lib, dst_lib, dirs_exist_ok=True)

def setup_windows():
    log("Setting up Windows libraries...")
    win_dir = os.path.join(FFMPEG_DIR, "windows")
    if os.path.exists(win_dir):
        shutil.rmtree(win_dir)
    
    f_win = os.path.join(TEMP_DIR, "win_amd64.zip")
    if download_file(WINDOWS_AMD64_URL, f_win) and extract_archive(f_win, TEMP_DIR):
        extracted_root = [d for d in os.listdir(TEMP_DIR) if d.startswith("ffmpeg-n") and "win64" in d][0]
        src = os.path.join(TEMP_DIR, extracted_root)
        
        # Copy includes and libs
        shutil.copytree(os.path.join(src, "include"), os.path.join(win_dir, "include"))
        shutil.copytree(os.path.join(src, "lib"), os.path.join(win_dir, "lib"))
        # Also copy bin (dlls) to lib for convenience if needed, or keep separate
        shutil.copytree(os.path.join(src, "bin"), os.path.join(win_dir, "bin"))

def setup_mac():
    log("Setting up macOS libraries (via Brew)...")
    mac_dir = os.path.join(FFMPEG_DIR, "mac")
    if os.path.exists(mac_dir):
        shutil.rmtree(mac_dir)
    
    try:
        # Get brew prefix
        result = subprocess.run(["brew", "--prefix", "ffmpeg"], capture_output=True, text=True)
        if result.returncode != 0:
            log("FFmpeg not found via brew. Please install: brew install ffmpeg")
            return
        
        brew_ffmpeg_path = result.stdout.strip()
        log(f"Found brew ffmpeg at {brew_ffmpeg_path}")
        
        # Create symlink or copy. Copy is safer for portability if we move the folder, 
        # but symlink is better for updates.
        # Given "3rd" implies vendoring, let's symlink to keep it lightweight 
        # but pointing to the valid system install.
        # Wait, if we symlink 'mac' -> '/opt/homebrew/opt/ffmpeg', 
        # then 'mac/include' and 'mac/lib' exist automatically.
        
        os.symlink(brew_ffmpeg_path, mac_dir)
        log(f"Symlinked {mac_dir} -> {brew_ffmpeg_path}")
        
    except Exception as e:
        log(f"Error setting up Mac: {e}")

def main():
    if not os.path.exists(THIRD_PARTY_DIR):
        os.makedirs(THIRD_PARTY_DIR)
    
    if not os.path.exists(FFMPEG_DIR):
        os.makedirs(FFMPEG_DIR)

    if os.path.exists(TEMP_DIR):
        shutil.rmtree(TEMP_DIR)
    os.makedirs(TEMP_DIR)

    # Always setup Linux (for cross compilation)
    setup_linux()

    # If on Windows, setup Windows
    if platform.system() == "Windows":
        setup_windows()
    
    # If on Mac, setup Mac
    if platform.system() == "Darwin":
        setup_mac()
        
    # Optional: If user wants to setup Windows on Mac (for cross compile), we could enabled it.
    # For now, let's just enable Windows download if explicitly asked or simple logic.
    # Let's just download Windows anyway so the folder is complete? 
    # It's about 30MB.
    if platform.system() != "Windows":
        setup_windows()

    # Cleanup
    shutil.rmtree(TEMP_DIR)
    log("Done.")

if __name__ == "__main__":
    main()
