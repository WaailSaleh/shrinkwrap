export namespace backend {
	
	export class Settings {
	    bot_token: string;
	    chat_id: string;
	    ui_scale: string;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bot_token = source["bot_token"];
	        this.chat_id = source["chat_id"];
	        this.ui_scale = source["ui_scale"];
	    }
	}
	export class Vault {
	    id: number;
	    filename: string;
	    file_size: number;
	    chunk_count: number;
	    uploaded_at: string;
	    source_path: string;
	
	    static createFrom(source: any = {}) {
	        return new Vault(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.filename = source["filename"];
	        this.file_size = source["file_size"];
	        this.chunk_count = source["chunk_count"];
	        this.uploaded_at = source["uploaded_at"];
	        this.source_path = source["source_path"];
	    }
	}

}

