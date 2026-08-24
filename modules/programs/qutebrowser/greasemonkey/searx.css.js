// ==UserScript==
// @name    Userstyle (searchx.css)
// @match        *://search.tinfoilforest.nz/*
// ==/UserScript==
GM_addStyle(`
  * {
    font-family: Noto Sans, sans-serif !important;
    /*border-radius: 5px !important;*/
  }
  #q {
    border-radius: 5px 0 0 5px !important;
  }
  #send_search {
    border-radius: 0 5px 5px 0 !important;
  }
  :root {
    --color-base-font: var(--system-theme-fg) !important;
    --color-base-background: var(--system-theme-bg0) !important;
    --color-base-background-mobile: var(--system-theme-bg0) !important;
    --color-url-font: var(--system-theme-primary) !important;
    --color-url-visited-font: var(--system-theme-purple) !important;
    --color-header-background: var(--system-theme-bg_dim) !important;
    --color-header-border: var(--system-theme-bg0) !important;
    --color-footer-background: var(--system-theme-bg_dim) !important;
    --color-footer-border: var(--system-theme-bg0) !important;
    --color-sidebar-border: var(--system-theme-bg1) !important;
    --color-sidebar-font: var(--system-theme-fg) !important;
    --color-sidebar-background: var(--system-theme-bg0) !important;
    --color-backtotop-font: var(--system-theme-fg) !important;
    --color-backtotop-border: var(--system-theme-bg0) !important;
    --color-backtotop-background: var(--system-theme-bg_dim) !important;
    --color-btn-background: var(--system-theme-primary) !important;
    --color-btn-font: var(--system-theme-bg0) !important;
    --color-show-btn-background: var(--system-theme-bg1) !important;
    --color-show-btn-font: var(--system-theme-fg) !important;
    --color-search-border: var(--system-theme-bg1) !important;
    --color-search-shadow: none !important;
    --color-search-background: var(--system-theme-bg0) !important;
    --color-search-font: var(--system-theme-fg) !important;
    --color-search-background-hover: var(--system-theme-primary) !important;
    --color-error: var(--system-theme-red) !important;
    --color-error-background: var(--system-theme-bg_red) !important;
    --color-warning: var(--system-theme-yellow) !important;
    --color-warning-background: var(--system-theme-bg_yellow) !important;
    --color-success: var(--system-theme-green) !important;
    --color-success-background: var(--system-theme-bg_green) !important;
    --color-categories-item-selected-font: var(--system-theme-primary) !important;
    --color-categories-item-border-selected: var(--system-theme-primary) !important;
    --color-autocomplete-font: var(--system-theme-fg) !important;
    --color-autocomplete-border: var(--system-theme-bg1) !important;
    --color-autocomplete-shadow: none !important;
    --color-autocomplete-background: var(--system-theme-bg_dim) !important;
    --color-autocomplete-background-hover: var(--system-theme-bg_dim) !important;
    --color-answer-font: var(--system-theme-fg) !important;
    --color-answer-background: var(--system-theme-bg0) !important;
    --color-result-background: var(--system-theme-bg0) !important;
    --color-result-border: var(--system-theme-bg0) !important;
    --color-result-url-font: var(--system-theme-fg) !important;
    --color-result-vim-selected: var(--system-theme-bg_visual) !important;
    --color-result-vim-arrow: var(--system-theme-primary) !important;
    --color-result-description-highlight-font: var(--system-theme-fg) !important;
    --color-result-link-font: var(--system-theme-primary) !important;
    --color-result-link-font-highlight: var(--system-theme-primary) !important;
    --color-result-link-visited-font: var(--system-theme-purple) !important;
    --color-result-publishdate-font: var(--system-theme-grey2) !important;
    --color-result-engines-font: var(--system-theme-grey2) !important;
    --color-result-search-url-border: var(--system-theme-bg1) !important;
    --color-result-search-url-font: var(--system-theme-fg) !important;
    --color-result-detail-font: var(--system-theme-fg) !important;
    --color-result-detail-label-font: var(--system-theme-grey2) !important;
    --color-result-detail-background: var(--system-theme-bg0) !important;
    --color-result-detail-hr: var(--system-theme-bg1) !important;
    --color-result-detail-link: var(--system-theme-primary) !important;
    --color-result-detail-loader-border: rgba(255, 255, 255, 0.2);
    --color-result-detail-loader-borderleft: rgba(0, 0, 0, 0);
    --color-result-image-span-font: var(--system-theme-fg) !important;
    --color-result-image-span-font-selected: var(--system-theme-bg0) !important;
    --color-result-image-background: var(--system-theme-bg0) !important;
    --color-settings-tr-hover: var(--system-theme-bg1) !important;
    --color-settings-engine-description-font: var(--system-theme-grey2) !important;
    --color-settings-table-group-background: var(--system-theme-bg_dim) !important;
    --color-toolkit-badge-font: var(--system-theme-fg) !important;
    --color-toolkit-badge-background: var(--system-theme-bg1) !important;
    --color-toolkit-kbd-font: var(--system-theme-bg0) !important;
    --color-toolkit-kbd-background: var(--system-theme-fg) !important;
    --color-toolkit-dialog-border: var(--system-theme-bg1) !important;
    --color-toolkit-dialog-background: var(--system-theme-bg_dim) !important;
    --color-toolkit-tabs-label-border: var(--system-theme-bg0) !important;
    --color-toolkit-tabs-section-border: var(--system-theme-bg1) !important;
    --color-toolkit-select-background: var(--system-theme-bg1) !important;
    --color-toolkit-select-border: var(--system-theme-bg1) !important;
    --color-toolkit-select-background-hover: var(--system-theme-bg2) !important;
    --color-toolkit-input-text-font: var(--system-theme-fg) !important;
    --color-toolkit-checkbox-onoff-off-background: var(--system-theme-bg1) !important;
    --color-toolkit-checkbox-onoff-on-background: var(--system-theme-bg1) !important;
    --color-toolkit-checkbox-onoff-on-mark-background: var(--system-theme-primary) !important;
    --color-toolkit-checkbox-onoff-on-mark-color: var(--system-theme-bg0) !important;
    --color-toolkit-checkbox-onoff-off-mark-background: var(--system-theme-fg) !important;
    --color-toolkit-checkbox-onoff-off-mark-color: var(--system-theme-bg0) !important;
    --color-toolkit-checkbox-label-background: var(--system-theme-bg0) !important;
    --color-toolkit-checkbox-label-border: var(--system-theme-bg0) !important;
    --color-toolkit-checkbox-input-border: var(--system-theme-primary) !important;
    --color-toolkit-engine-tooltip-border: var(--system-theme-bg0) !important;
    --color-toolkit-engine-tooltip-background: var(--system-theme-bg0) !important;
    --color-toolkit-loader-border: rgba(255, 255, 255, 0.2);
    --color-toolkit-loader-borderleft: rgba(0, 0, 0, 0);
    --color-doc-code: var(--system-theme-fg) !important;
    --color-doc-code-background: var(--system-theme-bg2) !important;
  }
`)

// just for fun, use my colourscheme for the search logo
const svgElement = document.querySelector("#search_logo > svg");
if (svgElement) {
  svgElement.querySelector("circle").setAttribute('stroke', 'var(--system-theme-primary)');
  svgElement.querySelector("path").setAttribute('stroke', 'var(--system-theme-primary)');
  svgElement.querySelector("rect").setAttribute('fill', 'var(--system-theme-primary)');
}
