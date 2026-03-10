{ config, ... }:
{
  # Setting my abbreviations
  home-manager.users.${config.nx.username}.programs.neovim = {
    extraConfig =
      # vim
      ''
        " markdown
        ab ch - [ ]
        " fractions
        ab 1o2 ½
        ab 1o4 ¼
        ab 3o4 ¾
        ab 1o3 ⅓
        ab 2o3 ⅔
        ab 1o5 ⅕
        ab 2o5 ⅖
        ab 3o5 ⅗
        ab 4o5 ⅘
        ab 1o6 ⅙
        ab 5o6 ⅚
        ab 1o8 ⅛
        ab 3o8 ⅜
        ab 5o8 ⅝
        ab 7o8 ⅞
        " superscript
        ab 1u ¹
        ab 2u ²
        ab 3u ³
        ab 4u ⁴
        ab 5u ⁵
        ab 6u ⁶
        ab 7u ⁷
        ab 8u ⁸
        ab 9u ⁹
        ab 0u ⁰
        " subscript
        ab 1d ₁
        ab 2d ₂
        ab 3d ₃
        ab 4d ₄
        ab 5d ₅
        ab 6d ₆
        ab 7d ₇
        ab 8d ₈
        ab 9d ₉
        ab 0d ₀
        " math
        ab pi π
        ab inf ∞
        " words with accents
        ab jalapeno jalapeño
        ab jalapenos jalapeños
        ab banh bánh
        ab mi mì
        ab cafe café
        ab kowhai kōwhai
        ab Kowhai Kōwhai
        ab enso ensō
        ab xeo xèo
        ab saute sauté
        ab poneke Pōneke
        ab maori Māori
        ab tuatara tuatārā
        ab tui tūī
        ab pohutukawa pōhutukawa
        ab belen Belén
        ab taupo Taupō
        ab otari Ōtari
        ab aioli aïoli
        ab puree purée
        ab creme crème
        ab fraiche fraîche
      '';
  };
}
