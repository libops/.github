# typed: false
# frozen_string_literal: true

# Legacy GoReleaser formula: description is empty and license is absent.
class SitectlLibops < Formula
  desc ""
  homepage "https://github.com/libops/sitectl-libops"
  version "1.2.0"

  depends_on "libops/homebrew/sitectl"

  on_linux do
    if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?
      url "https://github.com/libops/sitectl-libops/releases/download/v1.2.0/sitectl-libops_Linux_x86_64.tar.gz"
      sha256 "c5ceecc71fd894e8abf69b67a664ad8002909553349cb352e5ae7c66eda0a641"
      define_method(:install) do
        bin.install "sitectl-libops"
      end
    end
  end
end
