# typed: false
# frozen_string_literal: true

# Legacy GoReleaser formula: description is empty and license is absent.
class SitectlAppTmpl < Formula
  desc ""
  homepage "https://github.com/libops/sitectl-app-tmpl"
  version "0.1.2"

  depends_on "libops/homebrew/sitectl"

  on_linux do
    if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?
      url "https://github.com/libops/sitectl-app-tmpl/releases/download/v0.1.2/sitectl-app-tmpl_Linux_x86_64.tar.gz"
      sha256 "18fc316f85245102880b03a28dc5c7830b1b0cd6163c8657d8a0da3be40604b3"
      define_method(:install) do
        bin.install "sitectl-app-tmpl"
      end
    end
  end
end
