export namespace main {
	
	export class Config {
	    relay_url: string;
	    username: string;
	    password: string;
	    share_dir: string;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.relay_url = source["relay_url"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.share_dir = source["share_dir"];
	    }
	}
	export class DaemonStatus {
	    running: boolean;
	    connected: boolean;
	    username: string;
	    share_dir: string;
	    relay_url: string;
	
	    static createFrom(source: any = {}) {
	        return new DaemonStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.connected = source["connected"];
	        this.username = source["username"];
	        this.share_dir = source["share_dir"];
	        this.relay_url = source["relay_url"];
	    }
	}

}

