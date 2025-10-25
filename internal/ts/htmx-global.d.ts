// Minimal global declaration for the htmx CDN script
declare const htmx: {
    ajax(method: string, url: string, options?: { target?: string; swap?: string }): Promise<void>;
};


