"""Generate a source-built Homebrew formula for one release archive."""

import argparse
import hashlib
from pathlib import Path
import re


def render_formula(version: str, archive: Path) -> str:
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?", version):
        raise ValueError("expected a release version such as 0.1.0 or 0.1.0-rc.1")
    if archive.name != f"errand_{version}_source.tar.gz":
        raise ValueError("source archive name must match the release version")
    sha256 = hashlib.sha256(archive.read_bytes()).hexdigest()
    return f'''class Errand < Formula
  desc "Personal job runner for machines you own"
  homepage "https://github.com/lydakis/errand"
  url "https://github.com/lydakis/errand/releases/download/v{version}/{archive.name}"
  sha256 "{sha256}"
  license "MIT"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{{version}}"), "./cmd/errand"
  end

  def caveats
    <<~EOS
      On a runner, use `errand setup` to configure and manage the service.
      After upgrading, run `errand setup` when the runner is idle to restart it.
    EOS
  end

  test do
    assert_equal "errand #{{version}}", shell_output("#{{bin}}/errand version").strip
    assert_match "errand peers", shell_output("#{{bin}}/errand --help")
  end
end
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("archive", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    try:
        formula = render_formula(args.version, args.archive)
    except (ValueError, OSError) as error:
        parser.error(str(error))
    args.output.write_text(formula)


if __name__ == "__main__":
    main()
