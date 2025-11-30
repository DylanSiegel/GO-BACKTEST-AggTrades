import os
import sys
import gzip
import fnmatch
from pathlib import Path

# --- Configuration ---

SCRIPT_FILENAME = os.path.basename(__file__)  # Name of this script
OUTPUT_FILENAME = "context_output.txt"
ROOT_DIR = "."

# Directories to ignore
IGNORE_DIRS = {".venv", ".git", "__pycache__"}
IGNORE_FILE_PATTERNS = {"~", ".swp", ".DS_Store"}

# Relevant Extensions
PY_EXT = ".py"
CSV_EXT = ".csv"
CSV_GZ_EXT = ".csv.gz"
PARQUET_EXT = ".parquet"
YAML_EXT1 = ".yaml"
YAML_EXT2 = ".yml"
ENV_EXT = ".env"
SAFETENSORS_EXT = ".safetensors"
PT_EXT = ".pt"
JSON_EXT = ".json"
CU_EXT = ".cu"
CUH_EXT = ".cuh"
NPZ_EXT = ".npz"
GO_EXT = ".go"

# --- Helpers ---

def is_date_subdir(path_str):
    """
    Checks if path is a date-based subdir under bars_* or btc_parquet_clean.
    Matches logic: base starts with 'd=' and parent is 'bars_*' or 'btc_parquet_clean'.
    """
    path = Path(path_str)
    base = path.name
    if not base.startswith("d="):
        return False
    parent = path.parent.name
    return parent.startswith("bars_") or parent == "btc_parquet_clean"

def get_language_identifier(filename):
    """Returns markdown language tag based on extension."""
    lower = filename.lower()
    
    # Handle .gz extension stripping
    if lower.endswith(".gz"):
        ext = os.path.splitext(os.path.splitext(lower)[0])[1]
    else:
        ext = os.path.splitext(lower)[1]

    mapping = {
        ".py": "python",
        ".yaml": "yaml", ".yml": "yaml",
        ".env": "bash",
        ".csv": "csv",
        ".go": "go",
        ".js": "javascript",
        ".ts": "typescript",
        ".java": "java",
        ".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp",
        ".c": "c",
        ".cs": "csharp",
        ".rb": "ruby",
        ".php": "php",
        ".swift": "swift",
        ".rs": "rust",
        ".sh": "bash",
        ".sql": "sql",
        ".json": "json",
        ".xml": "xml",
        ".html": "html",
        ".css": "css",
        ".md": "markdown",
        ".cu": "cpp", ".cuh": "cpp",
        ".npz": "text"
    }
    return mapping.get(ext, "")

def get_file_content(path, full=True, n=0):
    """
    Reads file content. Handles .gz transparently.
    Uses 'utf-8' with error replacement to avoid crashing on binary data.
    """
    try:
        lower = path.lower()
        if lower.endswith(".gz"):
            open_func = gzip.open
        else:
            open_func = open

        with open_func(path, 'rt', encoding='utf-8', errors='replace') as f:
            if full:
                return f.read()
            else:
                lines = []
                for i, line in enumerate(f):
                    if i >= n: break
                    lines.append(line)
                
                content = "".join(lines)
                if i == 0:
                    content += "# (File is empty or could not be read)\n"
                elif i < n:
                    content += f"# (File has only {i} lines)\n"
                return content
    except Exception as e:
        return f"Error reading file: {e}"

def is_relevant_file(name):
    """Determines if a file is relevant based on extension."""
    lower = name.lower()

    # Skip temp files and output file
    if name == SCRIPT_FILENAME or name == OUTPUT_FILENAME:
        return False
    
    for pattern in IGNORE_FILE_PATTERNS:
        if pattern in name: # Simple substring check for temp files
            return False

    if lower.endswith(CSV_GZ_EXT):
        return True

    ext = os.path.splitext(lower)[1]
    valid_exts = {
        PY_EXT, GO_EXT, CSV_EXT, PARQUET_EXT, 
        YAML_EXT1, YAML_EXT2, ENV_EXT, 
        SAFETENSORS_EXT, PT_EXT, JSON_EXT, 
        CU_EXT, CUH_EXT, NPZ_EXT
    }
    return ext in valid_exts

# --- Main ---

def main():
    print("Starting: building file tree and collecting relevant paths...", file=sys.stderr)

    try:
        outfile = open(OUTPUT_FILENAME, 'w', encoding='utf-8')
    except IOError as e:
        print(f"Failed to create {OUTPUT_FILENAME}: {e}", file=sys.stderr)
        return

    relevant_paths = []

    # 1. Build Tree
    outfile.write("--- File Tree Structure ---\n")

    # os.walk yields (dirpath, dirnames, filenames)
    # We sort them to ensure deterministic output similar to file explorers
    for root, dirs, files in os.walk(ROOT_DIR, topdown=True):
        # Filter Directories in-place to stop os.walk from entering them
        dirs[:] = [d for d in dirs if d not in IGNORE_DIRS and not is_date_subdir(os.path.join(root, d))]
        dirs.sort()
        files.sort()

        # Calculate depth for indentation
        rel_path = os.path.relpath(root, ROOT_DIR)
        if rel_path == ".":
            depth = 0
        else:
            depth = rel_path.count(os.sep) + 1

        # Print current directory files (we don't print the directory itself here, 
        # usually tree view prints items *inside* the current walk)
        
        # However, to match the Go logic which visits items linearly:
        # We simulate the tree printing by calculating indentation based on path
        
        # Iterate dirs to print tree structure (directories)
        for d in dirs:
            indent = "    " * depth + "|-- "
            outfile.write(f"{indent}{d}/\n")

        # Iterate files to print tree structure
        for f in files:
            if f == SCRIPT_FILENAME or f == OUTPUT_FILENAME:
                continue
            
            # Skip temp files
            is_temp = False
            for pat in IGNORE_FILE_PATTERNS:
                if f.endswith(pat): is_temp = True
            if is_temp: continue

            if is_relevant_file(f):
                indent = "    " * depth + "|-- "
                outfile.write(f"{indent}{f}\n")
                full_path = os.path.join(root, f)
                relevant_paths.append(full_path)

    outfile.write("\n")
    outfile.flush()

    print(f"Tree build complete. Relevant files: {len(relevant_paths)}", file=sys.stderr)
    print("Processing file contents for context...", file=sys.stderr)

    # 2. Dump Content
    for path in relevant_paths:
        lower = path.lower()
        
        # Skip content dump for large/binary formats
        if (lower.endswith(CSV_EXT) or 
            lower.endswith(CSV_GZ_EXT) or 
            lower.endswith(PARQUET_EXT) or 
            lower.endswith(NPZ_EXT)):
            continue

        # Decide to read full content
        should_read_full = (
            lower.endswith(PY_EXT) or 
            lower.endswith(GO_EXT) or
            lower.endswith(YAML_EXT1) or 
            lower.endswith(YAML_EXT2) or
            lower.endswith(ENV_EXT) or 
            lower.endswith(CU_EXT) or 
            lower.endswith(CUH_EXT) or
            lower.endswith(SAFETENSORS_EXT) or 
            lower.endswith(PT_EXT) or
            lower.endswith(JSON_EXT)
        )

        if not should_read_full:
            continue

        rel_path = os.path.relpath(path, ROOT_DIR)
        lang_id = get_language_identifier(path)
        
        print(f"Including file: {rel_path}", file=sys.stderr)
        
        outfile.write(f"// --- File: {rel_path} ---\n\n")
        
        content = get_file_content(path)
        
        outfile.write(f"```{lang_id}\n")
        outfile.write(content)
        if content and not content.endswith("\n"):
            outfile.write("\n")
        outfile.write("```\n\n")
        
        outfile.write(f"// --- End File: {rel_path} ---\n\n")
        outfile.flush()

    outfile.close()
    print("File content collection complete.", file=sys.stderr)
    print(f"Successfully wrote context to {OUTPUT_FILENAME}", file=sys.stderr)

if __name__ == "__main__":
    main()