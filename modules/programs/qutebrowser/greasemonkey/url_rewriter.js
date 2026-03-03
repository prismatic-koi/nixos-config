// ==UserScript==
// @name     Universal URL Rewriter
// @version  1.2
// @grant    none
// @include  *
// @run-at   document-start
// ==/UserScript==

(function() {
    'use strict';
    
    // ============================================================
    // CONFIGURATION: Add your URL rewrite rules here
    // ============================================================
    // Each rule is an object with:
    //   - pattern: regex pattern to match URLs (string or RegExp)
    //   - replacement: replacement string or function
    //   - hosts: (optional) array of hostnames where this rule applies
    //            if omitted, rule applies to all sites
    // ============================================================
    
    const rewriteRules = [
        // Remove WordPress CDN prefix (i0-i3.wp.com proxies images poorly)
        {
            pattern: /https?:\/\/i[0-3]\.wp\.com\//,
            replacement: 'https://',
        },
        
        // Example: Add more rules here as needed
        // {
        //     pattern: /old-cdn\.example\.com/,
        //     replacement: 'new-cdn.example.com',
        //     hosts: ['example.com']
        // },
    ];
    
    // ============================================================
    // END CONFIGURATION
    // ============================================================
    
    // Apply all matching rewrite rules to a URL
    function rewriteUrl(url) {
        if (!url) return url;
        
        const currentHost = window.location.hostname;
        let rewrittenUrl = url;
        
        rewriteRules.forEach(function(rule) {
            // Check if rule applies to current host
            if (rule.hosts && rule.hosts.length > 0) {
                const hostMatches = rule.hosts.some(function(host) {
                    return currentHost === host || currentHost.endsWith('.' + host);
                });
                if (!hostMatches) return;
            }
            
            // Apply the rewrite
            const pattern = typeof rule.pattern === 'string' ? new RegExp(rule.pattern, 'g') : rule.pattern;
            rewrittenUrl = rewrittenUrl.replace(pattern, rule.replacement);
        });
        
        return rewrittenUrl;
    }
    
    // Apply rewrite rules to each URL in a srcset attribute value
    function rewriteSrcset(srcset) {
        if (!srcset) return srcset;
        return srcset.split(',').map(function(entry) {
            var parts = entry.trim().split(/\s+/);
            parts[0] = rewriteUrl(parts[0]);
            return parts.join(' ');
        }).join(', ');
    }
    
    // Intercept image requests
    const originalImage = window.Image;
    window.Image = function() {
        const img = new originalImage();
        const originalSrcSetter = Object.getOwnPropertyDescriptor(HTMLImageElement.prototype, 'src').set;
        
        Object.defineProperty(img, 'src', {
            set: function(value) {
                originalSrcSetter.call(this, rewriteUrl(value));
            },
            get: function() {
                return this.getAttribute('src');
            }
        });
        
        return img;
    };
    
    // Intercept setAttribute for all elements
    const originalSetAttribute = Element.prototype.setAttribute;
    Element.prototype.setAttribute = function(name, value) {
        if ((name === 'src' || name === 'href') && typeof value === 'string') {
            value = rewriteUrl(value);
        } else if (name === 'srcset' && typeof value === 'string') {
            value = rewriteSrcset(value);
        }
        return originalSetAttribute.call(this, name, value);
    };
    
    // Fix existing resources on page load
    window.addEventListener('DOMContentLoaded', function() {
        // Fix images (src and srcset)
        document.querySelectorAll('img[src], img[srcset]').forEach(function(img) {
            const newSrc = rewriteUrl(img.src);
            if (newSrc !== img.src) {
                img.src = newSrc;
            }
            var srcset = img.getAttribute('srcset');
            if (srcset) {
                var newSrcset = rewriteSrcset(srcset);
                if (newSrcset !== srcset) {
                    img.setAttribute('srcset', newSrcset);
                }
            }
        });
        
        // Fix source elements (used in <picture>)
        document.querySelectorAll('source[srcset]').forEach(function(source) {
            var srcset = source.getAttribute('srcset');
            if (srcset) {
                var newSrcset = rewriteSrcset(srcset);
                if (newSrcset !== srcset) {
                    source.setAttribute('srcset', newSrcset);
                }
            }
        });
        
        // Fix links
        document.querySelectorAll('a[href]').forEach(function(link) {
            const newHref = rewriteUrl(link.href);
            if (newHref !== link.href) {
                link.href = newHref;
            }
        });
        
        // Fix iframes
        document.querySelectorAll('iframe[src]').forEach(function(iframe) {
            const newSrc = rewriteUrl(iframe.src);
            if (newSrc !== iframe.src) {
                iframe.src = newSrc;
            }
        });
    });
    
    // Observe for dynamically added content
    const observer = new MutationObserver(function(mutations) {
        mutations.forEach(function(mutation) {
            mutation.addedNodes.forEach(function(node) {
                if (node.nodeType !== 1) return; // Only process element nodes
                
                // Check the node itself
                if (node.src) {
                    const newSrc = rewriteUrl(node.src);
                    if (newSrc !== node.src) {
                        node.src = newSrc;
                    }
                }
                if (node.href) {
                    const newHref = rewriteUrl(node.href);
                    if (newHref !== node.href) {
                        node.href = newHref;
                    }
                }
                var srcset = node.getAttribute && node.getAttribute('srcset');
                if (srcset) {
                    var newSrcset = rewriteSrcset(srcset);
                    if (newSrcset !== srcset) {
                        node.setAttribute('srcset', newSrcset);
                    }
                }
                
                // Check descendants
                if (node.querySelectorAll) {
                    node.querySelectorAll('img[src], img[srcset], source[srcset], a[href], iframe[src]').forEach(function(elem) {
                        if (elem.src) {
                            const newSrc = rewriteUrl(elem.src);
                            if (newSrc !== elem.src) {
                                elem.src = newSrc;
                            }
                        }
                        if (elem.href) {
                            const newHref = rewriteUrl(elem.href);
                            if (newHref !== elem.href) {
                                elem.href = newHref;
                            }
                        }
                        var eSrcset = elem.getAttribute('srcset');
                        if (eSrcset) {
                            var eNewSrcset = rewriteSrcset(eSrcset);
                            if (eNewSrcset !== eSrcset) {
                                elem.setAttribute('srcset', eNewSrcset);
                            }
                        }
                    });
                }
            });
        });
    });
    
    observer.observe(document.documentElement, {
        childList: true,
        subtree: true
    });
})();
